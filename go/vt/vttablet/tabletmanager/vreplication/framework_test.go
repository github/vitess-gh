/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vreplication

import (
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	_flag "vitess.io/vitess/go/internal/flag"
	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/binlog/binlogplayer"
	"vitess.io/vitess/go/vt/dbconfigs"
	"vitess.io/vitess/go/vt/grpcclient"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/servenv"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/vttablet/queryservice"
	"vitess.io/vitess/go/vt/vttablet/queryservice/fakes"
	"vitess.io/vitess/go/vt/vttablet/tabletconn"
	"vitess.io/vitess/go/vt/vttablet/tabletconntest"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/vstreamer"
	"vitess.io/vitess/go/vt/vttablet/tabletserver/vstreamer/testenv"
	"vitess.io/vitess/go/vt/withddl"

	binlogdatapb "vitess.io/vitess/go/vt/proto/binlogdata"
	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
)

var (
	playerEngine          *Engine
	streamerEngine        *vstreamer.Engine
	env                   *testenv.Env
	globalFBC             = &fakeBinlogClient{}
	vrepldb               = "vrepl"
	globalDBQueries       = make(chan string, 1000)
	testForeignKeyQueries = false
	doNotLogDBQueries     = false
)

type LogExpectation struct {
	Type   string
	Detail string
}

var heartbeatRe *regexp.Regexp

// setFlag() sets a flag for a test in a non-racy way:
//   - it registers the flag using a different flagset scope
//   - clears other flags by passing a dummy os.Args() while parsing this flagset
//   - sets the specific flag, if it has not already been defined
//   - resets the os.Args() so that the remaining flagsets can be parsed correctly
func setFlag(flagName, flagValue string) {
	flagSetName := "vreplication-unit-test"
	var tmp []string
	tmp, os.Args = os.Args[:], []string{flagSetName}
	defer func() { os.Args = tmp }()

	servenv.OnParseFor(flagSetName, func(fs *pflag.FlagSet) {
		if fs.Lookup(flagName) != nil {
			fmt.Printf("found %s: %+v", flagName, fs.Lookup(flagName).Value)
			return
		}
	})
	servenv.ParseFlags(flagSetName)

	if err := pflag.Set(flagName, flagValue); err != nil {
		msg := "failed to set flag %q to %q: %v"
		log.Errorf(msg, flagName, flagValue, err)
	}
}

func init() {
	tabletconn.RegisterDialer("test", func(tablet *topodatapb.Tablet, failFast grpcclient.FailFast) (queryservice.QueryService, error) {
		return &fakeTabletConn{
			QueryService: fakes.ErrorQueryService,
			tablet:       tablet,
		}, nil
	})
	tabletconntest.SetProtocol("go.vt.vttablet.tabletmanager.vreplication.framework_test", "test")

	binlogplayer.RegisterClientFactory("test", func() binlogplayer.Client { return globalFBC })
	heartbeatRe = regexp.MustCompile(`update _vt.vreplication set time_updated=\d+ where id=\d+`)
}

func TestMain(m *testing.M) {
	binlogplayer.SetProtocol("vreplication_test_framework", "test")
	_flag.ParseFlagsForTest()
	exitCode := func() int {
		var err error
		env, err = testenv.Init()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v", err)
			return 1
		}
		defer env.Close()

		vreplicationExperimentalFlags = 0

		// engines cannot be initialized in testenv because it introduces
		// circular dependencies.
		streamerEngine = vstreamer.NewEngine(env.TabletEnv, env.SrvTopo, env.SchemaEngine, nil, env.Cells[0])
		streamerEngine.InitDBConfig(env.KeyspaceName, env.ShardName)
		streamerEngine.Open()
		defer streamerEngine.Close()

		if err := env.Mysqld.ExecuteSuperQuery(context.Background(), fmt.Sprintf("create database %s", vrepldb)); err != nil {
			fmt.Fprintf(os.Stderr, "%v", err)
			return 1
		}

		if err := env.Mysqld.ExecuteSuperQuery(context.Background(), "set @@global.innodb_lock_wait_timeout=1"); err != nil {
			fmt.Fprintf(os.Stderr, "%v", err)
			return 1
		}

		externalConfig := map[string]*dbconfigs.DBConfigs{
			"exta": env.Dbcfgs,
			"extb": env.Dbcfgs,
		}
		playerEngine = NewTestEngine(env.TopoServ, env.Cells[0], env.Mysqld, realDBClientFactory, realDBClientFactory, vrepldb, externalConfig)
		playerEngine.Open(context.Background())
		defer playerEngine.Close()
		if err := env.Mysqld.ExecuteSuperQueryList(context.Background(), binlogplayer.CreateVReplicationTable()); err != nil {
			fmt.Fprintf(os.Stderr, "%v", err)
			return 1
		}

		for _, query := range binlogplayer.AlterVReplicationTable {
			env.Mysqld.ExecuteSuperQuery(context.Background(), query)
		}

		if err := env.Mysqld.ExecuteSuperQuery(context.Background(), createCopyState); err != nil {
			fmt.Fprintf(os.Stderr, "%v", err)
			return 1
		}

		if err := env.Mysqld.ExecuteSuperQuery(context.Background(), createVReplicationLogTable); err != nil {
			fmt.Fprintf(os.Stderr, "%v", err)
			return 1
		}

		return m.Run()
	}()
	os.Exit(exitCode)
}

func resetBinlogClient() {
	globalFBC = &fakeBinlogClient{}
}

func primaryPosition(t *testing.T) string {
	t.Helper()
	pos, err := env.Mysqld.PrimaryPosition()
	if err != nil {
		t.Fatal(err)
	}
	return mysql.EncodePosition(pos)
}

func execStatements(t *testing.T, queries []string) {
	t.Helper()
	if err := env.Mysqld.ExecuteSuperQueryList(context.Background(), queries); err != nil {
		log.Errorf("Error executing query: %s", err.Error())
		t.Error(err)
	}
}

//--------------------------------------
// Topos and tablets

func addTablet(id int) *topodatapb.Tablet {
	tablet := &topodatapb.Tablet{
		Alias: &topodatapb.TabletAlias{
			Cell: env.Cells[0],
			Uid:  uint32(id),
		},
		Keyspace: env.KeyspaceName,
		Shard:    env.ShardName,
		KeyRange: &topodatapb.KeyRange{},
		Type:     topodatapb.TabletType_REPLICA,
		PortMap: map[string]int32{
			"test": int32(id),
		},
	}
	if err := env.TopoServ.CreateTablet(context.Background(), tablet); err != nil {
		panic(err)
	}
	env.SchemaEngine.Reload(context.Background())
	return tablet
}

func addOtherTablet(id int, keyspace, shard string) *topodatapb.Tablet {
	tablet := &topodatapb.Tablet{
		Alias: &topodatapb.TabletAlias{
			Cell: env.Cells[0],
			Uid:  uint32(id),
		},
		Keyspace: keyspace,
		Shard:    shard,
		KeyRange: &topodatapb.KeyRange{},
		Type:     topodatapb.TabletType_REPLICA,
		PortMap: map[string]int32{
			"test": int32(id),
		},
	}
	if err := env.TopoServ.CreateTablet(context.Background(), tablet); err != nil {
		panic(err)
	}
	env.SchemaEngine.Reload(context.Background())
	return tablet
}

func deleteTablet(tablet *topodatapb.Tablet) {
	env.TopoServ.DeleteTablet(context.Background(), tablet.Alias)
	// This is not automatically removed from shard replication, which results in log spam.
	topo.DeleteTabletReplicationData(context.Background(), env.TopoServ, tablet)
	env.SchemaEngine.Reload(context.Background())
}

// fakeTabletConn implement TabletConn interface. We only care about the
// health check part. The state reported by the tablet will depend
// on the Tag values "serving" and "healthy".
type fakeTabletConn struct {
	queryservice.QueryService
	tablet *topodatapb.Tablet
}

// StreamHealth is part of queryservice.QueryService.
func (ftc *fakeTabletConn) StreamHealth(ctx context.Context, callback func(*querypb.StreamHealthResponse) error) error {
	return callback(&querypb.StreamHealthResponse{
		Serving: true,
		Target: &querypb.Target{
			Keyspace:   ftc.tablet.Keyspace,
			Shard:      ftc.tablet.Shard,
			TabletType: ftc.tablet.Type,
		},
		RealtimeStats: &querypb.RealtimeStats{},
	})
}

// vstreamHook allows you to do work just before calling VStream.
var vstreamHook func(ctx context.Context)

// VStream directly calls into the pre-initialized engine.
func (ftc *fakeTabletConn) VStream(ctx context.Context, request *binlogdatapb.VStreamRequest, send func([]*binlogdatapb.VEvent) error) error {
	if request.Target.Keyspace != "vttest" {
		<-ctx.Done()
		return io.EOF
	}
	if vstreamHook != nil {
		vstreamHook(ctx)
	}
	return streamerEngine.Stream(ctx, request.Position, request.TableLastPKs, request.Filter, send)
}

// vstreamRowsHook allows you to do work just before calling VStreamRows.
var vstreamRowsHook func(ctx context.Context)

// vstreamRowsSendHook allows you to do work just before VStreamRows calls send.
var vstreamRowsSendHook func(ctx context.Context)

// VStreamRows directly calls into the pre-initialized engine.
func (ftc *fakeTabletConn) VStreamRows(ctx context.Context, request *binlogdatapb.VStreamRowsRequest, send func(*binlogdatapb.VStreamRowsResponse) error) error {
	if vstreamRowsHook != nil {
		vstreamRowsHook(ctx)
	}
	var row []sqltypes.Value
	if request.Lastpk != nil {
		r := sqltypes.Proto3ToResult(request.Lastpk)
		if len(r.Rows) != 1 {
			return fmt.Errorf("unexpected lastpk input: %v", request.Lastpk)
		}
		row = r.Rows[0]
	}
	return streamerEngine.StreamRows(ctx, request.Query, row, func(rows *binlogdatapb.VStreamRowsResponse) error {
		if vstreamRowsSendHook != nil {
			vstreamRowsSendHook(ctx)
		}
		return send(rows)
	})
}

//--------------------------------------
// Binlog Client to TabletManager

// fakeBinlogClient satisfies binlogplayer.Client.
// Not to be used concurrently.
type fakeBinlogClient struct {
	lastTablet   *topodatapb.Tablet
	lastPos      string
	lastTables   []string
	lastKeyRange *topodatapb.KeyRange
	lastCharset  *binlogdatapb.Charset
}

func (fbc *fakeBinlogClient) Dial(tablet *topodatapb.Tablet) error {
	fbc.lastTablet = tablet
	return nil
}

func (fbc *fakeBinlogClient) Close() {
}

func (fbc *fakeBinlogClient) StreamTables(ctx context.Context, position string, tables []string, charset *binlogdatapb.Charset) (binlogplayer.BinlogTransactionStream, error) {
	fbc.lastPos = position
	fbc.lastTables = tables
	fbc.lastCharset = charset
	return &btStream{ctx: ctx}, nil
}

func (fbc *fakeBinlogClient) StreamKeyRange(ctx context.Context, position string, keyRange *topodatapb.KeyRange, charset *binlogdatapb.Charset) (binlogplayer.BinlogTransactionStream, error) {
	fbc.lastPos = position
	fbc.lastKeyRange = keyRange
	fbc.lastCharset = charset
	return &btStream{ctx: ctx}, nil
}

// btStream satisfies binlogplayer.BinlogTransactionStream
type btStream struct {
	ctx  context.Context
	sent bool
}

func (bts *btStream) Recv() (*binlogdatapb.BinlogTransaction, error) {
	if !bts.sent {
		bts.sent = true
		return &binlogdatapb.BinlogTransaction{
			Statements: []*binlogdatapb.BinlogTransaction_Statement{
				{
					Category: binlogdatapb.BinlogTransaction_Statement_BL_INSERT,
					Sql:      []byte("insert into t values(1)"),
				},
			},
			EventToken: &querypb.EventToken{
				Timestamp: 72,
				Position:  "MariaDB/0-1-1235",
			},
		}, nil
	}
	<-bts.ctx.Done()
	return nil, bts.ctx.Err()
}

func expectFBCRequest(t *testing.T, tablet *topodatapb.Tablet, pos string, tables []string, kr *topodatapb.KeyRange) {
	t.Helper()
	if !proto.Equal(tablet, globalFBC.lastTablet) {
		t.Errorf("Request tablet: %v, want %v", globalFBC.lastTablet, tablet)
	}
	if pos != globalFBC.lastPos {
		t.Errorf("Request pos: %v, want %v", globalFBC.lastPos, pos)
	}
	if !reflect.DeepEqual(tables, globalFBC.lastTables) {
		t.Errorf("Request tables: %v, want %v", globalFBC.lastTables, tables)
	}
	if !proto.Equal(kr, globalFBC.lastKeyRange) {
		t.Errorf("Request KeyRange: %v, want %v", globalFBC.lastKeyRange, kr)
	}
}

//--------------------------------------
// DBCLient wrapper

func realDBClientFactory() binlogplayer.DBClient {
	return &realDBClient{}
}

type realDBClient struct {
	conn  *mysql.Conn
	nolog bool
}

func (dbc *realDBClient) DBName() string {
	return vrepldb
}

func (dbc *realDBClient) Connect() error {
	app, err := env.Dbcfgs.AppWithDB().MysqlParams()
	if err != nil {
		return err
	}
	app.DbName = vrepldb
	conn, err := mysql.Connect(context.Background(), app)
	if err != nil {
		return err
	}
	dbc.conn = conn
	return nil
}

func (dbc *realDBClient) Begin() error {
	_, err := dbc.ExecuteFetch("begin", 10000)
	return err
}

func (dbc *realDBClient) Commit() error {
	_, err := dbc.ExecuteFetch("commit", 10000)
	return err
}

func (dbc *realDBClient) Rollback() error {
	_, err := dbc.ExecuteFetch("rollback", 10000)
	return err
}

func (dbc *realDBClient) Close() {
	dbc.conn.Close()
}

func (dbc *realDBClient) ExecuteFetch(query string, maxrows int) (*sqltypes.Result, error) {
	if strings.HasPrefix(query, "use") ||
		query == withddl.QueryToTriggerWithDDL { // this query breaks unit tests since it errors out
		return nil, nil
	}
	qr, err := dbc.conn.ExecuteFetch(query, 10000, true)
	if doNotLogDBQueries {
		return qr, err
	}
	if !strings.HasPrefix(query, "select") && !strings.HasPrefix(query, "set") && !dbc.nolog {
		globalDBQueries <- query
	} else if testForeignKeyQueries && strings.Contains(query, "foreign_key_checks") { //allow select/set for foreign_key_checks
		globalDBQueries <- query
	}
	return qr, err
}

func expectDeleteQueries(t *testing.T) {
	t.Helper()
	expectNontxQueries(t, []string{
		"/delete from _vt.vreplication",
		"/delete from _vt.copy_state",
	})
}

func expectLogsAndUnsubscribe(t *testing.T, logs []LogExpectation, logCh chan any) {
	t.Helper()
	defer vrLogStatsLogger.Unsubscribe(logCh)
	failed := false
	for i, log := range logs {
		if failed {
			t.Errorf("no logs received")
			continue
		}
		select {
		case data := <-logCh:
			got, ok := data.(*VrLogStats)
			if !ok {
				t.Errorf("got not ok casting to VrLogStats: %v", data)
			}
			var match bool
			match = (log.Type == got.Type)
			if match {
				if log.Detail[0] == '/' {
					result, err := regexp.MatchString(log.Detail[1:], got.Detail)
					if err != nil {
						panic(err)
					}
					match = result
				} else {
					match = (got.Detail == log.Detail)
				}
			}

			if !match {
				t.Errorf("log:\n%q, does not match log %d:\n%q", got, i, log)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("no logs received, expecting %s", log)
			failed = true
		}
	}
}

func shouldIgnoreQuery(query string) bool {
	queriesToIgnore := []string{
		"_vt.vreplication_log",   // ignore all selects, updates and inserts into this table
		"@@session.sql_mode",     // ignore all selects, and sets of this variable
		", time_heartbeat=",      // update of last heartbeat time, can happen out-of-band, so can't test for it
		", time_throttled=",      // update of last throttle time, can happen out-of-band, so can't test for it
		", component_throttled=", // update of last throttle time, can happen out-of-band, so can't test for it
		"context cancel",
	}
	for _, q := range queriesToIgnore {
		if strings.Contains(query, q) {
			return true
		}
	}
	return heartbeatRe.MatchString(query)
}

func expectDBClientQueries(t *testing.T, queries []string, skippableOnce ...string) {
	extraQueries := withDDL.DDLs()
	extraQueries = append(extraQueries, withDDLInitialQueries...)
	// Either 'queries' or 'queriesWithDDLs' must match globalDBQueries
	t.Helper()
	failed := false
	skippedOnce := false

	queryMatch := func(query string, got string) bool {
		if query[0] == '/' {
			result, err := regexp.MatchString(query[1:], got)
			if err != nil {
				panic(err)
			}
			return result
		}
		return (got == query)
	}
	for i, query := range queries {
		if failed {
			t.Errorf("no query received, expecting %s", query)
			continue
		}
		var got string
	retry:
		select {
		case got = <-globalDBQueries:
			// We rule out heartbeat time update queries because otherwise our query list
			// is indeterminable and varies with each test execution.
			if shouldIgnoreQuery(got) {
				goto retry
			}
			for _, extraQuery := range extraQueries {
				if got == extraQuery {
					goto retry
				}
			}

			if !queryMatch(query, got) {
				if !skippedOnce {
					// let's see if "got" is a skippable query
					for _, skippable := range skippableOnce {
						if queryMatch(skippable, got) {
							skippedOnce = true
							goto retry
						}
					}
				}
				t.Errorf("query:\n%q, does not match expected query %d:\n%q", got, i, query)
			}
		case <-time.After(5 * time.Second):
			t.Errorf("no query received, expecting %s", query)
			failed = true
		}
	}
	for {
		select {
		case got := <-globalDBQueries:
			if shouldIgnoreQuery(got) {
				continue
			}
			t.Errorf("unexpected query: %s", got)
		default:
			return
		}
	}
}

// expectNontxQueries disregards transactional statements like begin and commit.
// It also disregards updates to _vt.vreplication.
func expectNontxQueries(t *testing.T, queries []string) {
	t.Helper()
	failed := false

	skipQueries := withDDLInitialQueries
	skipQueries = append(skipQueries, withDDL.DDLs()...)
	for i, query := range queries {
		if failed {
			t.Errorf("no query received, expecting %s", query)
			continue
		}
		var got string
	retry:
		select {
		case got = <-globalDBQueries:
			if got == "begin" || got == "commit" || got == "rollback" || strings.Contains(got, "update _vt.vreplication set pos") ||
				shouldIgnoreQuery(got) {
				goto retry
			}
			for _, skipQuery := range skipQueries {
				if got == skipQuery {
					goto retry
				}
			}

			var match bool
			if query[0] == '/' {
				result, err := regexp.MatchString(query[1:], got)
				if err != nil {
					panic(err)
				}
				match = result
			} else {
				match = (got == query)
			}
			require.True(t, match, "query %d:: got:%s, want:%s", i, got, query)
		case <-time.After(5 * time.Second):
			t.Fatalf("no query received, expecting %s", query)
			failed = true
		}
	}
	for {
		select {
		case got := <-globalDBQueries:
			if got == "begin" || got == "commit" || got == "rollback" || strings.Contains(got, "_vt.vreplication") {
				continue
			}
			if shouldIgnoreQuery(got) {
				continue
			}
			t.Errorf("unexpected query: %s", got)
		default:
			return
		}
	}
}
func expectData(t *testing.T, table string, values [][]string) {
	t.Helper()
	customExpectData(t, table, values, env.Mysqld.FetchSuperQuery)
}

func expectQueryResult(t *testing.T, query string, values [][]string) {
	t.Helper()
	err := compareQueryResults(t, query, values, env.Mysqld.FetchSuperQuery)
	if err != nil {
		require.FailNow(t, "data mismatch", err)
	}
}

func customExpectData(t *testing.T, table string, values [][]string, exec func(ctx context.Context, query string) (*sqltypes.Result, error)) {
	t.Helper()

	var query string
	if len(strings.Split(table, ".")) == 1 {
		query = fmt.Sprintf("select * from %s.%s", vrepldb, table)
	} else {
		query = fmt.Sprintf("select * from %s", table)
	}
	err := compareQueryResults(t, query, values, exec)
	if err != nil {
		require.FailNow(t, "data mismatch", err)
	}
}

func compareQueryResults(t *testing.T, query string, values [][]string,
	exec func(ctx context.Context, query string) (*sqltypes.Result, error)) error {

	t.Helper()
	qr, err := exec(context.Background(), query)
	if err != nil {
		return err
	}
	if len(values) != len(qr.Rows) {
		return fmt.Errorf("row counts don't match: %v, want %v", qr.Rows, values)
	}
	for i, row := range values {
		if len(row) != len(qr.Rows[i]) {
			return fmt.Errorf("Too few columns, \nrow: %d, \nresult: %d:%v, \nwant: %d:%v", i, len(qr.Rows[i]), qr.Rows[i], len(row), row)
		}
		for j, val := range row {
			if got := qr.Rows[i][j].ToString(); got != val {
				return fmt.Errorf("Mismatch at (%d, %d): %v, want %s", i, j, qr.Rows[i][j], val)
			}
		}
	}

	return nil
}

func validateQueryCountStat(t *testing.T, phase string, want int64) {
	var count int64
	for _, ct := range globalStats.status().Controllers {
		for ph, cnt := range ct.QueryCounts {
			if ph == phase {
				count += cnt
			}
		}
	}
	require.Equal(t, want, count, "QueryCount stat is incorrect")
}

func validateCopyRowCountStat(t *testing.T, want int64) {
	var count int64
	for _, ct := range globalStats.status().Controllers {
		count += ct.CopyRowCount
	}
	require.Equal(t, want, count, "CopyRowCount stat is incorrect")
}
