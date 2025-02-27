// Copyright 2020 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/parser"
	"github.com/pingcap/tidb/parser/ast"
	"github.com/pingcap/tidb/parser/charset"
	"github.com/pingcap/tidb/parser/terror"
	_ "github.com/pingcap/tidb/parser/test_driver"
	log "github.com/sirupsen/logrus"
)

var (
	wg       sync.WaitGroup
	host     string
	port     string
	user     string
	passwd   string
	logLevel string
	record   bool
	params   string
	all      bool
)

func init() {
	flag.StringVar(&host, "host", "127.0.0.1", "The host of the TiDB/MySQL server.")
	flag.StringVar(&port, "port", "4000", "The listen port of TiDB/MySQL server.")
	flag.StringVar(&user, "user", "root", "The user for connecting to the database.")
	flag.StringVar(&passwd, "passwd", "", "The password for the user.")
	flag.StringVar(&logLevel, "log-level", "error", "The log level of mysql-tester: info, warn, error, debug.")
	flag.BoolVar(&record, "record", false, "Whether to record the test output to the result file.")
	flag.StringVar(&params, "params", "", "Additional params pass as DSN(e.g. session variable)")
	flag.BoolVar(&all, "all", false, "run all tests")

	c := &charset.Charset{
		Name:             "gbk",
		DefaultCollation: "gbk_bin",
		Collations:       map[string]*charset.Collation{},
	}
	charset.AddCharset(c)
	for _, coll := range charset.GetCollations() {
		if strings.EqualFold(coll.CharsetName, c.Name) {
			charset.AddCollation(coll)
		}
	}
}

const (
	default_connection = "default"
)

type query struct {
	firstWord string
	Query     string
	Line      int
	tp        int
}

type Conn struct {
	mdb *sql.DB
	tx  *sql.Tx
}

type tester struct {
	mdb  *sql.DB
	name string

	tx *sql.Tx

	buf bytes.Buffer

	// enable query log will output origin statement into result file too
	// use --disable_query_log or --enable_query_log to control it
	enableQueryLog bool

	singleQuery bool

	// sortedResult make the output or the current query sorted.
	sortedResult bool

	enableConcurrent bool
	// Disable or enable warnings. This setting is enabled by default.
	// With this setting enabled, mysqltest uses SHOW WARNINGS to display
	// any warnings produced by SQL statements.
	enableWarning bool

	// check expected error, use --error before the statement
	// see http://dev.mysql.com/doc/mysqltest/2.0/en/writing-tests-expecting-errors.html
	expectedErrs []string

	// only for test, not record, every time we execute a statement, we should read the result
	// data to check correction.
	resultFD *os.File
	// ctx is used for Compile sql statement
	ctx *parser.Parser

	// conns record connection created by test.
	conn map[string]*Conn

	// currConnName record current connection name.
	currConnName string
}

func newTester(name string) *tester {
	t := new(tester)

	t.name = name
	t.enableQueryLog = true
	// disable warning by default since our a lot of test cases
	// are ported wihtout explictly "disablewarning"
	t.enableWarning = false
	t.enableConcurrent = false
	t.ctx = parser.New()
	t.ctx.EnableWindowFunc(true)

	return t
}

func setHashJoinConcurrency(db *sql.DB) {
	if _, err := db.Exec("SET @@tidb_hash_join_concurrency=1"); err != nil {
		log.Fatalf("Executing \"SET @@tidb_hash_join_concurrency=1\" err[%v]", err)
	}
}

// isTiDB returns true if the DB is confirmed to be TiDB
func isTiDB(db *sql.DB) bool {
	if _, err := db.Exec("SELECT tidb_version()"); err != nil {
		log.Infof("This doesn't look like a TiDB server, err[%v]", err)
		return false
	}
	return true
}

func (t *tester) addConnection(connName, hostName, userName, password, db string) {
	mdb, err := OpenDBWithRetry("mysql", userName+":"+password+"@tcp("+hostName+":"+port+")/"+db+"?time_zone=%27Asia%2FShanghai%27"+params)
	if err != nil {
		log.Fatalf("Open db err %v", err)
	}
	if isTiDB(mdb) {
		if _, err = mdb.Exec("SET @@tidb_init_chunk_size=1"); err != nil {
			log.Fatalf("Executing \"SET @@tidb_init_chunk_size=1\" err[%v]", err)
		}
		if _, err = mdb.Exec("SET @@tidb_max_chunk_size=32"); err != nil {
			log.Fatalf("Executing \"SET @@tidb_max_chunk_size=32\" err[%v]", err)
		}
		setHashJoinConcurrency(mdb)
	}
	t.conn[connName] = &Conn{mdb: mdb, tx: nil}
	t.switchConnection(connName)
}

func (t *tester) switchConnection(connName string) {
	conn, ok := t.conn[connName]
	if !ok {
		log.Fatalf("Connection %v doesn't exist.", connName)
	}
	// save current connection.
	t.conn[t.currConnName].tx = t.tx
	// switch connection.
	t.mdb = conn.mdb
	t.tx = conn.tx
	t.currConnName = connName
}

func (t *tester) disconnect(connName string) {
	conn, ok := t.conn[connName]
	if !ok {
		log.Fatalf("Connection %v doesn't exist.", connName)
	}
	err := conn.mdb.Close()
	if err != nil {
		log.Fatal(err)
	}
	delete(t.conn, connName)
	t.mdb = t.conn[default_connection].mdb
	t.tx = t.conn[default_connection].tx
	t.currConnName = default_connection
}

func (t *tester) preProcess() {
	dbName := "test"
	mdb, err := OpenDBWithRetry("mysql", user+":"+passwd+"@tcp("+host+":"+port+")/"+dbName+"?time_zone=%27Asia%2FShanghai%27"+params)
	t.conn = make(map[string]*Conn)
	if err != nil {
		log.Fatalf("Open db err %v", err)
	}

	log.Warn("Create new db", mdb)

	if _, err = mdb.Exec(fmt.Sprintf("create database `%s`", t.name)); err != nil {
		log.Fatalf("Executing create db %s err[%v]", t.name, err)
	}

	if _, err = mdb.Exec(fmt.Sprintf("use `%s`", t.name)); err != nil {
		log.Fatalf("Executing Use test err[%v]", err)
	}
	if isTiDB(mdb) {
		if _, err = mdb.Exec("SET @@tidb_init_chunk_size=1"); err != nil {
			log.Fatalf("Executing \"SET @@tidb_init_chunk_size=1\" err[%v]", err)
		}
		if _, err = mdb.Exec("SET @@tidb_max_chunk_size=32"); err != nil {
			log.Fatalf("Executing \"SET @@tidb_max_chunk_size=32\" err[%v]", err)
		}
		setHashJoinConcurrency(mdb)
	}
	t.mdb = mdb
	t.conn[default_connection] = &Conn{mdb: mdb, tx: nil}
	t.currConnName = default_connection
}

func (t *tester) postProcess() {
	t.mdb.Exec(fmt.Sprintf("drop database `%s`", t.name))
	for _, v := range t.conn {
		v.mdb.Close()
	}
}

func (t *tester) Run() error {
	t.preProcess()
	defer t.postProcess()
	queries, err := t.loadQueries()
	if err != nil {
		return errors.Trace(err)
	}

	if err = t.openResult(); err != nil {
		return errors.Trace(err)
	}

	var s string
	defer func() {
		if t.tx != nil {
			log.Errorf("transaction is not committed correctly, rollback")
			t.rollback()
		}

		if t.resultFD != nil {
			t.resultFD.Close()
		}
	}()

	testCnt := 0
	startTime := time.Now()
	var concurrentQueue []query
	var concurrentSize int
	for _, q := range queries {
		s = q.Query
		switch q.tp {
		case Q_ENABLE_QUERY_LOG:
			t.enableQueryLog = true
		case Q_DISABLE_QUERY_LOG:
			t.enableQueryLog = false
		case Q_DISABLE_WARNINGS:
			t.enableWarning = false
		case Q_ENABLE_WARNINGS:
			t.enableWarning = true
		case Q_SINGLE_QUERY:
			t.singleQuery = true
		case Q_BEGIN_CONCURRENT:
			concurrentQueue = make([]query, 0)
			t.enableConcurrent = true
			if s == "" {
				concurrentSize = 8
			} else {
				concurrentSize, err = strconv.Atoi(strings.TrimSpace(s))
				if err != nil {
					return errors.Annotate(err, "Atoi failed")
				}
			}
		case Q_END_CONCURRENT:
			t.enableConcurrent = false
			if err = t.concurrentRun(concurrentQueue, concurrentSize); err != nil {
				return errors.Annotate(err, fmt.Sprintf("concurrent test failed in %v", t.name))
			}
			t.expectedErrs = nil
		case Q_ERROR:
			t.expectedErrs = strings.Split(strings.TrimSpace(s), ",")
		case Q_ECHO:
			t.buf.WriteString(s)
			t.buf.WriteString("\n")
		case Q_QUERY:
			if t.enableConcurrent {
				concurrentQueue = append(concurrentQueue, q)
			} else if err = t.execute(q); err != nil {
				return errors.Annotate(err, fmt.Sprintf("sql:%v", q.Query))
			}

			testCnt++

			t.sortedResult = false
		case Q_SORTED_RESULT:
			t.sortedResult = true
		case Q_CONNECT:
			q.Query = strings.TrimSpace(q.Query)
			if q.Query[len(q.Query)-1] == ';' {
				q.Query = q.Query[:len(q.Query)-1]
			}
			q.Query = q.Query[1 : len(q.Query)-1]
			args := strings.Split(q.Query, ",")
			for i := range args {
				args[i] = strings.TrimSpace(args[i])
			}
			for i := 0; i < 4; i++ {
				args = append(args, "")
			}
			t.addConnection(args[0], args[1], args[2], args[3], args[4])
		case Q_CONNECTION:
			q.Query = strings.TrimSpace(q.Query)
			if q.Query[len(q.Query)-1] == ';' {
				q.Query = q.Query[:len(q.Query)-1]
			}
			t.switchConnection(q.Query)
		case Q_DISCONNECT:
			q.Query = strings.TrimSpace(q.Query)
			if q.Query[len(q.Query)-1] == ';' {
				q.Query = q.Query[:len(q.Query)-1]
			}
			t.disconnect(q.Query)
		}
	}

	fmt.Printf("%s: ok! %d test cases passed, take time %v s\n", t.testFileName(), testCnt, time.Since(startTime).Seconds())

	return t.flushResult()
}

func (t *tester) concurrentRun(concurrentQueue []query, concurrentSize int) error {
	if len(concurrentQueue) == 0 {
		return nil
	}
	offset := t.buf.Len()

	if concurrentSize <= 0 {
		return errors.Errorf("concurrentSize must be positive")
	}
	if concurrentSize > len(concurrentQueue) {
		concurrentSize = len(concurrentQueue)
	}
	batchQuery := make([][]query, concurrentSize)
	for i, query := range concurrentQueue {
		j := i % concurrentSize
		batchQuery[j] = append(batchQuery[j], query)
	}
	errOccured := make(chan struct{}, len(concurrentQueue))
	var wg sync.WaitGroup
	wg.Add(len(batchQuery))
	for _, q := range batchQuery {
		go t.concurrentExecute(q, &wg, errOccured)
	}
	wg.Wait()
	close(errOccured)
	if _, ok := <-errOccured; ok {
		return errors.Errorf("Run failed")
	}
	buf := t.buf.Bytes()[:offset]
	t.buf = *(bytes.NewBuffer(buf))
	return nil
}

func (t *tester) concurrentExecute(querys []query, wg *sync.WaitGroup, errOccured chan struct{}) {
	defer wg.Done()
	tt := newTester(t.name)
	dbName := "test"
	mdb, err := OpenDBWithRetry("mysql", user+":"+passwd+"@tcp("+host+":"+port+")/"+dbName+"?time_zone=%27Asia%2FShanghai%27"+params)
	if err != nil {
		log.Fatalf("Open db err %v", err)
	}
	if _, err = mdb.Exec(fmt.Sprintf("use `%s`", t.name)); err != nil {
		log.Fatalf("Executing Use test err[%v]", err)
	}
	if isTiDB(mdb) {
		if _, err = mdb.Exec("SET @@tidb_init_chunk_size=1"); err != nil {
			log.Fatalf("Executing \"SET @@tidb_init_chunk_size=1\" err[%v]", err)
		}
		if _, err = mdb.Exec("SET @@tidb_max_chunk_size=32"); err != nil {
			log.Fatalf("Executing \"SET @@tidb_max_chunk_size=32\" err[%v]", err)
		}
		setHashJoinConcurrency(mdb)
	}
	tt.mdb = mdb
	defer tt.mdb.Close()

	for _, query := range querys {
		if len(query.Query) == 0 {
			return
		}
		list, _, err := tt.ctx.Parse(query.Query, "", "")
		if err != nil {
			msgs <- testTask{
				test: t.name,
				err:  t.parserErrorHandle(query, err),
			}
			errOccured <- struct{}{}
			return
		}

		for _, st := range list {
			err = tt.stmtExecute(query, st)
			if err != nil && len(t.expectedErrs) > 0 {
				for _, tStr := range t.expectedErrs {
					if strings.Contains(err.Error(), tStr) {
						err = nil
						break
					}
				}
			}
			t.singleQuery = false
			if err != nil {
				msgs <- testTask{
					test: t.name,
					err:  errors.Trace(errors.Errorf("run \"%v\" at line %d err %v", st.Text(), query.Line, err)),
				}
				errOccured <- struct{}{}
				return
			}

		}
	}
	return
}
func (t *tester) loadQueries() ([]query, error) {
	data, err := ioutil.ReadFile(t.testFileName())
	if err != nil {
		return nil, err
	}

	seps := bytes.Split(data, []byte("\n"))
	queries := make([]string, 0, len(seps))
	newStmt := true
	for _, v := range seps {
		v := bytes.TrimSpace(v)
		s := string(v)
		// we will skip # comment here
		if strings.HasPrefix(s, "#") {
			newStmt = true
			continue
		} else if strings.HasPrefix(s, "--") {
			queries = append(queries, s)
			newStmt = true
			continue
		} else if len(s) == 0 {
			continue
		}

		if newStmt {
			queries = append(queries, s)
		} else {
			lastQuery := queries[len(queries)-1]
			lastQuery = fmt.Sprintf("%s\n%s", lastQuery, s)
			queries[len(queries)-1] = lastQuery
		}

		// if the line has a ; in the end, we will treat new line as the new statement.
		newStmt = strings.HasSuffix(s, ";")
	}

	return ParseQueries(queries...)
}

func (t *tester) writeError(query query, err error) {
	if t.enableQueryLog {
		t.buf.WriteString(query.Query)
		t.buf.WriteString("\n")
	}

	t.buf.WriteString(fmt.Sprintf("%s\n", err))
}

// parserErrorHandle handle error from parser.Parse() function
// 1. If it's a syntax error, and `--error ER_PARSE_ERROR` enabled, record to result file
// 2. If it's a non-syntax error, and `--error xxxxx` enabled, record to result file
// 3. Otherwise, throw this error, stop running mysql-test
func (t *tester) parserErrorHandle(query query, err error) error {
	offset := t.buf.Len()
	// TODO: check whether this err is expected.
	if len(t.expectedErrs) > 0 {
		switch innerErr := errors.Cause(err).(type) {
		case *terror.Error:
			t.writeError(query, innerErr)
			err = nil
		}
	}
	err = syntaxError(err)
	for _, expectedErr := range t.expectedErrs {
		if expectedErr == "ER_PARSE_ERROR" {
			t.writeError(query, err)
			err = nil
			break
		}
	}

	if err != nil {
		return errors.Trace(err)
	}

	// clear expected errors after we execute the first query
	t.expectedErrs = nil
	t.singleQuery = false

	if !record {
		// check test result now
		gotBuf := t.buf.Bytes()[offset:]
		buf := make([]byte, t.buf.Len()-offset)
		if _, err = t.resultFD.ReadAt(buf, int64(offset)); err != nil {
			return errors.Trace(errors.Errorf("run \"%v\" at line %d err, we got \n%s\nbut read result err %s", query.Query, query.Line, gotBuf, err))
		}

		if !bytes.Equal(gotBuf, buf) {
			return errors.Trace(errors.Errorf("run \"%v\" around line %d err, we need(%v):\n%s\nbut got(%v):\n%s\n", query.Query, query.Line, len(buf), buf, len(gotBuf), gotBuf))
		}
	}

	return errors.Trace(err)
}

func syntaxError(err error) error {
	if err == nil {
		return nil
	}
	return parser.ErrParse.GenWithStackByArgs("You have an error in your SQL syntax; check the manual that corresponds to your TiDB version for the right syntax to use", err.Error())
}

func (t *tester) stmtExecute(query query, st ast.StmtNode) (err error) {

	var qText string
	if t.singleQuery {
		qText = query.Query
	} else {
		qText = st.Text()
	}
	if t.enableQueryLog {
		t.buf.WriteString(qText)
		t.buf.WriteString("\n")
	}
	switch st.(type) {
	case *ast.BeginStmt:
		t.tx, err = t.mdb.Begin()
		if err != nil {
			t.rollback()
			break
		}
	case *ast.CommitStmt:
		err = t.commit()
		if err != nil {
			t.rollback()
			break
		}
	case *ast.RollbackStmt:
		err = t.rollback()
		if err != nil {
			break
		}
	default:
		if t.tx != nil {
			err = t.executeStmt(qText)
			if err != nil {
				break
			}
		} else {
			// if begin or following commit fails, we don't think
			// this error is the expected one.
			if t.tx, err = t.mdb.Begin(); err != nil {
				t.rollback()
				break
			}

			err = t.executeStmt(qText)
			if err != nil {
				t.rollback()
				break
			} else {
				commitErr := t.commit()
				if err == nil && commitErr != nil {
					err = commitErr
				}
				if commitErr != nil {
					t.rollback()
					break
				}
			}
		}
	}
	return err
}

func (t *tester) execute(query query) error {
	if len(query.Query) == 0 {
		return nil
	}
	list, _, err := t.ctx.Parse(query.Query, "", "")
	if err != nil {
		return t.parserErrorHandle(query, err)
	}

	for _, st := range list {
		offset := t.buf.Len()
		err = t.stmtExecute(query, st)

		if err != nil && len(t.expectedErrs) > 0 {
			// TODO: check whether this err is expected.
			// but now we think it is.

			// output expected err
			fmt.Fprintf(&t.buf, "%s\n", strings.ReplaceAll(err.Error(), "\r", ""))
			err = nil
		}
		// clear expected errors after we execute the first query
		t.expectedErrs = nil
		t.singleQuery = false

		if err != nil {
			return errors.Trace(errors.Errorf("run \"%v\" at line %d err %v", st.Text(), query.Line, err))
		}

		if !record {
			// check test result now
			gotBuf := t.buf.Bytes()[offset:]

			buf := make([]byte, t.buf.Len()-offset)
			if _, err = t.resultFD.ReadAt(buf, int64(offset)); err != nil {
				return errors.Trace(errors.Errorf("run \"%v\" at line %d err, we got \n%s\nbut read result err %s", st.Text(), query.Line, gotBuf, err))
			}

			if !bytes.Equal(gotBuf, buf) {
				return errors.Trace(errors.Errorf("failed to run query \n\"%v\" \n around line %d, \nwe need(%v):\n%s\nbut got(%v):\n%s\n", query.Query, query.Line, len(buf), buf, len(gotBuf), gotBuf))
			}
		}
	}

	return errors.Trace(err)
}

func (t *tester) commit() error {
	err := t.tx.Commit()
	if err != nil {
		return err
	}
	t.tx = nil
	return nil
}

func (t *tester) rollback() error {
	if t.tx == nil {
		return nil
	}
	err := t.tx.Rollback()
	t.tx = nil
	return err
}

func (t *tester) writeQueryResult(rows *byteRows) error {
	cols := rows.cols
	for i, c := range cols {
		t.buf.WriteString(c)
		if i != len(cols)-1 {
			t.buf.WriteString("\t")
		}
	}
	t.buf.WriteString("\n")

	for _, row := range rows.data {
		var value string
		for i, col := range row.data {
			// Here we can check if the value is nil (NULL value)
			if col == nil {
				value = "NULL"
			} else {
				value = string(col)
			}
			t.buf.WriteString(value)
			if i < len(row.data)-1 {
				t.buf.WriteString("\t")
			}
		}
		t.buf.WriteString("\n")
	}
	return nil
}

type byteRow struct {
	data [][]byte
}

type byteRows struct {
	cols []string
	data []byteRow
}

func (rows *byteRows) Len() int {
	return len(rows.data)
}

func (rows *byteRows) Less(i, j int) bool {
	r1 := rows.data[i]
	r2 := rows.data[j]
	for i := 0; i < len(r1.data); i++ {
		res := bytes.Compare(r1.data[i], r2.data[i])
		switch res {
		case -1:
			return true
		case 1:
			return false
		}
	}
	return false
}

func (rows *byteRows) Swap(i, j int) {
	rows.data[i], rows.data[j] = rows.data[j], rows.data[i]
}

func dumpToByteRows(rows *sql.Rows) (*byteRows, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, errors.Trace(err)
	}

	data := make([]byteRow, 0, 8)
	args := make([]interface{}, len(cols))
	for rows.Next() {
		tmp := make([][]byte, len(cols))
		for i := 0; i < len(args); i++ {
			args[i] = &tmp[i]
		}
		err := rows.Scan(args...)
		if err != nil {
			return nil, errors.Trace(err)
		}

		data = append(data, byteRow{tmp})
	}
	err = rows.Err()
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &byteRows{cols: cols, data: data}, nil
}

func (t *tester) executeStmt(query string) error {
	if IsQuery(query) {
		raw, err := t.tx.Query(query)
		if err != nil {
			return errors.Trace(err)
		}

		rows, err := dumpToByteRows(raw)
		if err != nil {
			return errors.Trace(err)
		}

		if t.sortedResult {
			sort.Sort(rows)
		}

		return t.writeQueryResult(rows)
	} else {
		// TODO: rows affected and last insert id
		_, err := t.tx.Exec(query)
		if err != nil {
			return errors.Trace(err)
		}
	}
	return nil
}

func (t *tester) openResult() error {
	if record {
		return nil
	}

	var err error
	t.resultFD, err = os.Open(t.resultFileName())
	return err
}

func (t *tester) flushResult() error {
	if !record {
		return nil
	}
	return ioutil.WriteFile(t.resultFileName(), t.buf.Bytes(), 0644)
}

func (t *tester) testFileName() string {
	// test and result must be in current ./t the same as MySQL
	return fmt.Sprintf("./t/%s.test", t.name)
}

func (t *tester) resultFileName() string {
	// test and result must be in current ./r, the same as MySQL
	return fmt.Sprintf("./r/%s.result", t.name)
}

func loadAllTests() ([]string, error) {
	// tests must be in t folder
	files, err := ioutil.ReadDir("./t")
	if err != nil {
		return nil, err
	}

	tests := make([]string, 0, len(files))
	for _, f := range files {
		if f.IsDir() {
			continue
		}

		// the test file must have a suffix .test
		name := f.Name()
		if strings.HasSuffix(name, ".test") {
			name = strings.TrimSuffix(name, ".test")

			tests = append(tests, name)
		}
	}

	return tests, nil
}

func resultExists(name string) bool {
	resultFile := fmt.Sprintf("./r/%s.result", name)

	if _, err := os.Stat(resultFile); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// convertTestsToTestTasks convert all test cases into several testBatches.
// If we have 11 cases and batchSize is 5, then we will have 4 testBatches.
func convertTestsToTestTasks(tests []string) (tTasks []testBatch, have_show, have_is bool) {
	batchSize := 30
	total := (len(tests) / batchSize) + 2
	// the extra 1 is for sub_query_more test
	tTasks = make([]testBatch, total+1)
	testIdx := 0
	have_subqmore, have_role := false, false
	for i := 0; i < total; i++ {
		tTasks[i] = make(testBatch, 0, batchSize)
		for j := 0; j <= batchSize && testIdx < len(tests); j++ {
			// skip sub_query_more test, since it consumes the most time
			// we better use a separate goroutine to run it
			// role test has many connection/disconnection operation.
			// we better use a separate goroutine to run it
			switch tests[testIdx] {
			case "sub_query_more":
				have_subqmore = true
			case "show":
				have_show = true
			case "infoschema":
				have_is = true
			case "role":
				have_role = true
			case "role2":
				have_role = true
			default:
				tTasks[i] = append(tTasks[i], tests[testIdx])
			}
			testIdx++
		}
	}

	if have_subqmore {
		tTasks[total-1] = testBatch{"sub_query_more"}
	}

	if have_role {
		tTasks[total] = testBatch{"role", "role2"}
	}
	return
}

var msgs = make(chan testTask)

type testTask struct {
	err  error
	test string
}

type testBatch []string

func (t testBatch) Run() {
	defer wg.Done()
	for _, test := range t {
		tr := newTester(test)
		msgs <- testTask{
			test: test,
			err:  tr.Run(),
		}
	}
}

func (t testBatch) String() string {
	return strings.Join([]string(t), ", ")
}

func executeTests(tasks []testBatch, have_show, have_is bool) {
	// show and infoschema have to be executed first, since the following
	// tests will create database using their own name.
	if have_show {
		show := newTester("show")
		msgs <- testTask{
			test: "show",
			err:  show.Run(),
		}
	}

	if have_is {
		infoschema := newTester("infoschema")
		msgs <- testTask{
			test: "infoschema",
			err:  infoschema.Run(),
		}
	}

	wg.Add(len(tasks))
	for _, t := range tasks {
		go t.Run()
	}

	wg.Wait()
}

func consumeError() []error {
	var es []error
	for {
		if t, more := <-msgs; more {
			if t.err != nil {
				e := fmt.Errorf("run test [%s] err: %v", t.test, t.err)
				if !all {
					log.Fatalln(e)
				}
				log.Errorln(e)
				es = append(es, e)
			} else {
				log.Infof("run test [%s] ok", t.test)
			}

		} else {
			return es
		}
	}
}

func main() {
	flag.Parse()
	tests := flag.Args()

	// we will run all tests if no tests assigned
	if len(tests) == 0 {
		var err error
		if tests, err = loadAllTests(); err != nil {
			log.Fatalf("load all tests err %v", err)
		}
	}

	if !record {
		log.Infof("running tests: %v", tests)
	} else {
		log.Infof("recording tests: %v", tests)
	}

	go func() {
		executeTests(convertTestsToTestTasks(tests))
		close(msgs)
	}()

	es := consumeError()
	if len(es) != 0 {
		println()
		log.Errorf("%d tests failed\n", len(es))
		for _, item := range es {
			log.Errorln(item)
		}
		os.Exit(1)
	}

	println("\nGreat, All tests passed")
}
