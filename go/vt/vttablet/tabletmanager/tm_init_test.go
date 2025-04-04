/*
Copyright 2020 The Vitess Authors.

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

package tabletmanager

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/mysql/collations"
	"vitess.io/vitess/go/mysql/fakesqldb"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/sync2"
	"vitess.io/vitess/go/test/utils"
	"vitess.io/vitess/go/vt/dbconfigs"
	"vitess.io/vitess/go/vt/logutil"
	"vitess.io/vitess/go/vt/mysqlctl/fakemysqldaemon"
	"vitess.io/vitess/go/vt/servenv"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/memorytopo"
	"vitess.io/vitess/go/vt/topotools"
	"vitess.io/vitess/go/vt/vttablet/tabletservermock"

	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	vschemapb "vitess.io/vitess/go/vt/proto/vschema"
)

var (
	dbServerVersion = "5.7.0"
	charsetName     = "utf8mb4"
	dbsvCollID      = collations.NewEnvironment(dbServerVersion).DefaultCollationForCharset(charsetName).ID()
)

func TestStartBuildTabletFromInput(t *testing.T) {
	alias := &topodatapb.TabletAlias{
		Cell: "cell",
		Uid:  1,
	}
	port := int32(12)
	grpcport := int32(34)

	// Hostname should be used as is.
	tabletHostname = "foo"
	initKeyspace = "test_keyspace"
	initShard = "0"
	initTabletType = "replica"
	initDbNameOverride = "aa"
	wantTablet := &topodatapb.Tablet{
		Alias:    alias,
		Hostname: "foo",
		PortMap: map[string]int32{
			"vt":   port,
			"grpc": grpcport,
		},
		Keyspace:             "test_keyspace",
		Shard:                "0",
		KeyRange:             nil,
		Type:                 topodatapb.TabletType_REPLICA,
		Tags:                 map[string]string{},
		DbNameOverride:       "aa",
		DbServerVersion:      dbServerVersion,
		DefaultConnCollation: uint32(dbsvCollID),
	}

	gotTablet, err := BuildTabletFromInput(alias, port, grpcport, dbServerVersion, nil)
	require.NoError(t, err)

	// Hostname should be resolved.
	assert.Equal(t, wantTablet, gotTablet)
	tabletHostname = ""
	gotTablet, err = BuildTabletFromInput(alias, port, grpcport, dbServerVersion, nil)
	require.NoError(t, err)
	assert.NotEqual(t, "", gotTablet.Hostname)

	// Canonicalize shard name and compute keyrange.
	tabletHostname = "foo"
	initShard = "-C0"
	wantTablet.Shard = "-c0"
	wantTablet.KeyRange = &topodatapb.KeyRange{
		Start: []byte(""),
		End:   []byte("\xc0"),
	}
	gotTablet, err = BuildTabletFromInput(alias, port, grpcport, dbServerVersion, nil)
	require.NoError(t, err)
	// KeyRange check is explicit because the next comparison doesn't
	// show the diff well enough.
	assert.Equal(t, wantTablet.KeyRange, gotTablet.KeyRange)
	assert.Equal(t, wantTablet, gotTablet)

	// Invalid inputs.
	initKeyspace = ""
	initShard = "0"
	_, err = BuildTabletFromInput(alias, port, grpcport, dbServerVersion, nil)
	assert.Contains(t, err.Error(), "init_keyspace and init_shard must be specified")

	initKeyspace = "test_keyspace"
	initShard = ""
	_, err = BuildTabletFromInput(alias, port, grpcport, dbServerVersion, nil)
	assert.Contains(t, err.Error(), "init_keyspace and init_shard must be specified")

	initShard = "x-y"
	_, err = BuildTabletFromInput(alias, port, grpcport, dbServerVersion, nil)
	assert.Contains(t, err.Error(), "cannot validate shard name")

	initShard = "0"
	initTabletType = "bad"
	_, err = BuildTabletFromInput(alias, port, grpcport, dbServerVersion, nil)
	assert.Contains(t, err.Error(), "unknown TabletType bad")

	initTabletType = "primary"
	_, err = BuildTabletFromInput(alias, port, grpcport, dbServerVersion, nil)
	assert.Contains(t, err.Error(), "invalid init_tablet_type PRIMARY")
}

func TestBuildTabletFromInputWithBuildTags(t *testing.T) {
	alias := &topodatapb.TabletAlias{
		Cell: "cell",
		Uid:  1,
	}
	port := int32(12)
	grpcport := int32(34)

	// Hostname should be used as is.
	tabletHostname = "foo"
	initKeyspace = "test_keyspace"
	initShard = "0"
	initTabletType = "replica"
	initDbNameOverride = "aa"
	skipBuildInfoTags = ""
	defer func() { skipBuildInfoTags = "/.*/" }()
	wantTablet := &topodatapb.Tablet{
		Alias:    alias,
		Hostname: "foo",
		PortMap: map[string]int32{
			"vt":   port,
			"grpc": grpcport,
		},
		Keyspace:             "test_keyspace",
		Shard:                "0",
		KeyRange:             nil,
		Type:                 topodatapb.TabletType_REPLICA,
		Tags:                 servenv.AppVersion.ToStringMap(),
		DbNameOverride:       "aa",
		DbServerVersion:      dbServerVersion,
		DefaultConnCollation: uint32(dbsvCollID),
	}

	gotTablet, err := BuildTabletFromInput(alias, port, grpcport, dbServerVersion, nil)
	require.NoError(t, err)
	assert.Equal(t, wantTablet, gotTablet)
}

func TestStartCreateKeyspaceShard(t *testing.T) {
	defer func(saved time.Duration) { rebuildKeyspaceRetryInterval = saved }(rebuildKeyspaceRetryInterval)
	rebuildKeyspaceRetryInterval = 10 * time.Millisecond

	ctx := context.Background()
	statsTabletTypeCount.ResetAll()
	cell := "cell1"
	ts := memorytopo.NewServer(cell)
	tm := newTestTM(t, ts, 1, "ks", "0")
	defer tm.Stop()

	assert.Equal(t, "replica", statsTabletType.Get())
	assert.Equal(t, 1, len(statsTabletTypeCount.Counts()))
	assert.Equal(t, int64(1), statsTabletTypeCount.Counts()["replica"])

	_, err := ts.GetShard(ctx, "ks", "0")
	require.NoError(t, err)

	ensureSrvKeyspace(t, ts, cell, "ks")

	srvVSchema, err := ts.GetSrvVSchema(context.Background(), cell)
	require.NoError(t, err)
	wantVSchema := &vschemapb.Keyspace{}
	assert.Equal(t, wantVSchema, srvVSchema.Keyspaces["ks"])

	// keyspace-shard already created.
	_, err = ts.GetOrCreateShard(ctx, "ks1", "0")
	require.NoError(t, err)
	tm = newTestTM(t, ts, 2, "ks1", "0")
	defer tm.Stop()
	_, err = ts.GetShard(ctx, "ks1", "0")
	require.NoError(t, err)
	ensureSrvKeyspace(t, ts, cell, "ks1")
	srvVSchema, err = ts.GetSrvVSchema(context.Background(), cell)
	require.NoError(t, err)
	assert.Equal(t, wantVSchema, srvVSchema.Keyspaces["ks1"])

	// srvKeyspace already created
	_, err = ts.GetOrCreateShard(ctx, "ks2", "0")
	require.NoError(t, err)
	err = topotools.RebuildKeyspace(ctx, logutil.NewConsoleLogger(), ts, "ks2", []string{cell}, false)
	require.NoError(t, err)
	tm = newTestTM(t, ts, 3, "ks2", "0")
	defer tm.Stop()
	_, err = ts.GetShard(ctx, "ks2", "0")
	require.NoError(t, err)
	_, err = ts.GetSrvKeyspace(context.Background(), cell, "ks2")
	require.NoError(t, err)
	srvVSchema, err = ts.GetSrvVSchema(context.Background(), cell)
	require.NoError(t, err)
	assert.Equal(t, wantVSchema, srvVSchema.Keyspaces["ks2"])

	// srvVSchema already created
	_, err = ts.GetOrCreateShard(ctx, "ks3", "0")
	require.NoError(t, err)
	err = topotools.RebuildKeyspace(ctx, logutil.NewConsoleLogger(), ts, "ks3", []string{cell}, false)
	require.NoError(t, err)
	err = ts.RebuildSrvVSchema(ctx, []string{cell})
	require.NoError(t, err)
	tm = newTestTM(t, ts, 4, "ks3", "0")
	defer tm.Stop()
	_, err = ts.GetShard(ctx, "ks3", "0")
	require.NoError(t, err)
	_, err = ts.GetSrvKeyspace(context.Background(), cell, "ks3")
	require.NoError(t, err)
	srvVSchema, err = ts.GetSrvVSchema(context.Background(), cell)
	require.NoError(t, err)
	assert.Equal(t, wantVSchema, srvVSchema.Keyspaces["ks3"])

	// Multi-shard
	tm1 := newTestTM(t, ts, 5, "ks4", "-80")
	defer tm1.Stop()

	// Wait a bit and make sure that srvKeyspace is still not created.
	time.Sleep(100 * time.Millisecond)
	_, err = ts.GetSrvKeyspace(context.Background(), cell, "ks4")
	require.True(t, topo.IsErrType(err, topo.NoNode), err)

	tm2 := newTestTM(t, ts, 6, "ks4", "80-")
	defer tm2.Stop()
	// Now that we've started the tablet for the other shard, srvKeyspace will succeed.
	ensureSrvKeyspace(t, ts, cell, "ks4")
}

func TestCheckPrimaryShip(t *testing.T) {
	defer func(saved time.Duration) { rebuildKeyspaceRetryInterval = saved }(rebuildKeyspaceRetryInterval)
	rebuildKeyspaceRetryInterval = 10 * time.Millisecond

	ctx := context.Background()
	cell := "cell1"
	ts := memorytopo.NewServer(cell)
	alias := &topodatapb.TabletAlias{
		Cell: "cell1",
		Uid:  1,
	}

	// 1. Initialize the tablet as REPLICA.
	// This will create the respective topology records.
	tm := newTestTM(t, ts, 1, "ks", "0")
	tablet := tm.Tablet()
	ensureSrvKeyspace(t, ts, cell, "ks")
	ti, err := ts.GetTablet(ctx, alias)
	require.NoError(t, err)
	assert.Equal(t, topodatapb.TabletType_REPLICA, ti.Type)
	tm.Stop()

	// 2. Update shard's primary to our alias, then try to init again.
	// (This simulates the case where the PrimaryAlias in the shard record says
	// that we are the primary but the tablet record says otherwise. In that case,
	// we become primary by inheriting the shard record's timestamp.)
	now := time.Now()
	_, err = ts.UpdateShardFields(ctx, "ks", "0", func(si *topo.ShardInfo) error {
		si.PrimaryAlias = alias
		si.PrimaryTermStartTime = logutil.TimeToProto(now)
		// Reassign to now for easier comparison.
		now = si.GetPrimaryTermStartTime()
		return nil
	})
	require.NoError(t, err)
	err = tm.Start(tablet, 0)
	require.NoError(t, err)
	ti, err = ts.GetTablet(ctx, alias)
	require.NoError(t, err)
	assert.Equal(t, topodatapb.TabletType_PRIMARY, ti.Type)
	ter0 := ti.GetPrimaryTermStartTime()
	assert.Equal(t, now, ter0)
	assert.Equal(t, "primary", statsTabletType.Get())
	tm.Stop()

	// 3. Delete the tablet record. The shard record still says that we are the
	// PRIMARY. Since it is the only source, we assume that its information is
	// correct and start as PRIMARY.
	err = ts.DeleteTablet(ctx, alias)
	require.NoError(t, err)
	err = tm.Start(tablet, 0)
	require.NoError(t, err)
	ti, err = ts.GetTablet(ctx, alias)
	require.NoError(t, err)
	assert.Equal(t, topodatapb.TabletType_PRIMARY, ti.Type)
	ter1 := ti.GetPrimaryTermStartTime()
	tm.Stop()

	// 4. Fix the tablet record to agree that we're primary.
	// Shard and tablet record are in sync now and we assume that we are actually
	// the PRIMARY.
	ti.Type = topodatapb.TabletType_PRIMARY
	err = ts.UpdateTablet(ctx, ti)
	require.NoError(t, err)
	err = tm.Start(tablet, 0)
	require.NoError(t, err)
	ti, err = ts.GetTablet(ctx, alias)
	require.NoError(t, err)
	assert.Equal(t, topodatapb.TabletType_PRIMARY, ti.Type)
	ter2 := ti.GetPrimaryTermStartTime()
	assert.Equal(t, ter1, ter2)
	tm.Stop()

	// 5. Subsequent inits will still start the vttablet as PRIMARY.
	err = tm.Start(tablet, 0)
	require.NoError(t, err)
	ti, err = ts.GetTablet(ctx, alias)
	require.NoError(t, err)
	assert.Equal(t, topodatapb.TabletType_PRIMARY, ti.Type)
	ter3 := ti.GetPrimaryTermStartTime()
	assert.Equal(t, ter1, ter3)
	tm.Stop()

	// 6. If the shard record shows a different primary with an older
	// timestamp, we take over primaryship.
	otherAlias := &topodatapb.TabletAlias{
		Cell: "cell1",
		Uid:  2,
	}
	otherTablet := &topodatapb.Tablet{
		Alias:         otherAlias,
		Keyspace:      "ks",
		Shard:         "0",
		Type:          topodatapb.TabletType_PRIMARY,
		MysqlHostname: "localhost",
		MysqlPort:     1234,
	}
	// Create the tablet record for the primary
	err = ts.CreateTablet(ctx, otherTablet)
	require.NoError(t, err)
	_, err = ts.UpdateShardFields(ctx, "ks", "0", func(si *topo.ShardInfo) error {
		si.PrimaryAlias = otherAlias
		si.PrimaryTermStartTime = logutil.TimeToProto(ter1.Add(-10 * time.Second))
		return nil
	})
	require.NoError(t, err)
	err = tm.Start(tablet, 0)
	require.NoError(t, err)
	ti, err = ts.GetTablet(ctx, alias)
	require.NoError(t, err)
	assert.Equal(t, topodatapb.TabletType_PRIMARY, ti.Type)
	ter4 := ti.GetPrimaryTermStartTime()
	assert.Equal(t, ter1, ter4)
	tm.Stop()

	// 7. If the shard record shows a different primary with a newer
	// timestamp, we remain replica.
	_, err = ts.UpdateShardFields(ctx, "ks", "0", func(si *topo.ShardInfo) error {
		si.PrimaryAlias = otherAlias
		si.PrimaryTermStartTime = logutil.TimeToProto(ter4.Add(10 * time.Second))
		return nil
	})
	require.NoError(t, err)
	tablet.Type = topodatapb.TabletType_REPLICA
	tablet.PrimaryTermStartTime = nil
	// Get the fakeMySQL and set it up to expect a set replication source command
	fakeMysql := tm.MysqlDaemon.(*fakemysqldaemon.FakeMysqlDaemon)
	fakeMysql.SetReplicationSourceInputs = append(fakeMysql.SetReplicationSourceInputs, fmt.Sprintf("%v:%v", otherTablet.MysqlHostname, otherTablet.MysqlPort))
	fakeMysql.ExpectedExecuteSuperQueryList = []string{
		"STOP SLAVE",
		"RESET SLAVE ALL",
		"FAKE SET MASTER",
		"START SLAVE",
	}
	err = tm.Start(tablet, 0)
	require.NoError(t, err)
	ti, err = ts.GetTablet(ctx, alias)
	require.NoError(t, err)
	assert.Equal(t, topodatapb.TabletType_REPLICA, ti.Type)
	ter5 := ti.GetPrimaryTermStartTime()
	assert.True(t, ter5.IsZero())
	tm.Stop()
}

func TestStartCheckMysql(t *testing.T) {
	ctx := context.Background()
	cell := "cell1"
	ts := memorytopo.NewServer(cell)
	tablet := newTestTablet(t, 1, "ks", "0")
	cp := mysql.ConnParams{
		Host: "foo",
		Port: 1,
	}
	tm := &TabletManager{
		BatchCtx:            context.Background(),
		TopoServer:          ts,
		MysqlDaemon:         newTestMysqlDaemon(t, 1),
		DBConfigs:           dbconfigs.NewTestDBConfigs(cp, cp, ""),
		QueryServiceControl: tabletservermock.NewController(),
	}
	err := tm.Start(tablet, 0)
	require.NoError(t, err)
	defer tm.Stop()

	ti, err := ts.GetTablet(ctx, tm.tabletAlias)
	require.NoError(t, err)
	assert.Equal(t, int32(1), ti.MysqlPort)
	assert.Equal(t, "foo", ti.MysqlHostname)
}

func TestStartFindMysqlPort(t *testing.T) {
	defer func(saved time.Duration) { mysqlPortRetryInterval = saved }(mysqlPortRetryInterval)
	mysqlPortRetryInterval = 1 * time.Millisecond

	ctx := context.Background()
	cell := "cell1"
	ts := memorytopo.NewServer(cell)
	tablet := newTestTablet(t, 1, "ks", "0")
	fmd := newTestMysqlDaemon(t, -1)
	tm := &TabletManager{
		BatchCtx:            context.Background(),
		TopoServer:          ts,
		MysqlDaemon:         fmd,
		DBConfigs:           &dbconfigs.DBConfigs{},
		QueryServiceControl: tabletservermock.NewController(),
	}
	err := tm.Start(tablet, 0)
	require.NoError(t, err)
	defer tm.Stop()

	ti, err := ts.GetTablet(ctx, tm.tabletAlias)
	require.NoError(t, err)
	assert.Equal(t, int32(0), ti.MysqlPort)

	fmd.MysqlPort.Set(3306)
	for i := 0; i < 10; i++ {
		ti, err := ts.GetTablet(ctx, tm.tabletAlias)
		require.NoError(t, err)
		if ti.MysqlPort == 3306 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	assert.Fail(t, "mysql port was not updated")
}

// Init tablet fixes replication data when safe
func TestStartFixesReplicationData(t *testing.T) {
	ctx := context.Background()
	cell := "cell1"
	ts := memorytopo.NewServer(cell, "cell2")
	tm := newTestTM(t, ts, 1, "ks", "0")
	defer tm.Stop()
	tabletAlias := tm.tabletAlias

	sri, err := ts.GetShardReplication(ctx, cell, "ks", "0")
	require.NoError(t, err)
	utils.MustMatch(t, tabletAlias, sri.Nodes[0].TabletAlias)

	// Remove the ShardReplication record, try to create the
	// tablets again, make sure it's fixed.
	err = topo.RemoveShardReplicationRecord(ctx, ts, cell, "ks", "0", tabletAlias)
	require.NoError(t, err)
	sri, err = ts.GetShardReplication(ctx, cell, "ks", "0")
	require.NoError(t, err)
	assert.Equal(t, 0, len(sri.Nodes))

	// An initTablet will recreate the shard replication data.
	err = tm.initTablet(context.Background())
	require.NoError(t, err)

	sri, err = ts.GetShardReplication(ctx, cell, "ks", "0")
	require.NoError(t, err)
	utils.MustMatch(t, tabletAlias, sri.Nodes[0].TabletAlias)
}

// This is a test to make sure a regression does not happen in the future.
// There is code in Start that updates replication data if tablet fails
// to be created due to a NodeExists error. During this particular error we were not doing
// the sanity checks that the provided tablet was the same in the topo.
func TestStartDoesNotUpdateReplicationDataForTabletInWrongShard(t *testing.T) {
	ctx := context.Background()
	ts := memorytopo.NewServer("cell1", "cell2")
	tm := newTestTM(t, ts, 1, "ks", "0")
	tm.Stop()

	tabletAliases, err := ts.FindAllTabletAliasesInShard(ctx, "ks", "0")
	require.NoError(t, err)
	assert.Equal(t, uint32(1), tabletAliases[0].Uid)

	tablet := newTestTablet(t, 1, "ks", "-d0")
	require.NoError(t, err)
	err = tm.Start(tablet, 0)
	assert.Contains(t, err.Error(), "existing tablet keyspace and shard ks/0 differ")

	tablets, err := ts.FindAllTabletAliasesInShard(ctx, "ks", "-d0")
	require.NoError(t, err)
	assert.Equal(t, 0, len(tablets))
}

func TestCheckTabletTypeResets(t *testing.T) {
	defer func(saved time.Duration) { rebuildKeyspaceRetryInterval = saved }(rebuildKeyspaceRetryInterval)
	rebuildKeyspaceRetryInterval = 10 * time.Millisecond

	ctx := context.Background()
	cell := "cell1"
	ts := memorytopo.NewServer(cell)
	alias := &topodatapb.TabletAlias{
		Cell: "cell1",
		Uid:  1,
	}

	// 1. Initialize the tablet as REPLICA.
	// This will create the respective topology records.
	tm := newTestTM(t, ts, 1, "ks", "0")
	tablet := tm.Tablet()
	ensureSrvKeyspace(t, ts, cell, "ks")
	ti, err := ts.GetTablet(ctx, alias)
	require.NoError(t, err)
	assert.Equal(t, topodatapb.TabletType_REPLICA, ti.Type)
	tm.Stop()

	// 2. Update tablet record with tabletType RESTORE
	_, err = ts.UpdateTabletFields(ctx, alias, func(t *topodatapb.Tablet) error {
		t.Type = topodatapb.TabletType_RESTORE
		return nil
	})
	require.NoError(t, err)
	err = tm.Start(tablet, 0)
	require.NoError(t, err)
	assert.Equal(t, tm.tmState.tablet.Type, tm.tmState.displayState.tablet.Type)
	ti, err = ts.GetTablet(ctx, alias)
	require.NoError(t, err)
	// Verify that it changes back to initTabletType
	assert.Equal(t, topodatapb.TabletType_REPLICA, ti.Type)

	// 3. Update shard's primary to our alias, then try to init again.
	// (This simulates the case where the PrimaryAlias in the shard record says
	// that we are the primary but the tablet record says otherwise. In that case,
	// we become primary by inheriting the shard record's timestamp.)
	now := time.Now()
	_, err = ts.UpdateShardFields(ctx, "ks", "0", func(si *topo.ShardInfo) error {
		si.PrimaryAlias = alias
		si.PrimaryTermStartTime = logutil.TimeToProto(now)
		// Reassign to now for easier comparison.
		now = si.GetPrimaryTermStartTime()
		return nil
	})
	require.NoError(t, err)
	si, err := tm.createKeyspaceShard(ctx)
	require.NoError(t, err)
	err = tm.checkPrimaryShip(ctx, si)
	require.NoError(t, err)
	assert.Equal(t, tm.tmState.tablet.Type, tm.tmState.displayState.tablet.Type)
	err = tm.initTablet(ctx)
	require.NoError(t, err)
	assert.Equal(t, tm.tmState.tablet.Type, tm.tmState.displayState.tablet.Type)
	ti, err = ts.GetTablet(ctx, alias)
	require.NoError(t, err)
	assert.Equal(t, topodatapb.TabletType_PRIMARY, ti.Type)
	ter0 := ti.GetPrimaryTermStartTime()
	assert.Equal(t, now, ter0)
	tm.Stop()
}

func TestGetBuildTags(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in      map[string]string
		skipCSV string
		want    map[string]string
		wantErr bool
	}{
		{
			in: map[string]string{
				"a": "a",
				"b": "b",
				"c": "c",
			},
			skipCSV: "a,c",
			want: map[string]string{
				"b": "b",
			},
		},
		{
			in: map[string]string{
				"hello": "world",
				"help":  "me",
				"good":  "bye",
				"a":     "b",
			},
			skipCSV: "a,/hel.*/",
			want: map[string]string{
				"good": "bye",
			},
		},
		{
			in: map[string]string{
				"a":      "a",
				"/hello": "/hello",
			},
			skipCSV: "/,a", // len(skipTag) <= 1, so not a regexp
			want: map[string]string{
				"/hello": "/hello",
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.skipCSV, func(t *testing.T) {
			t.Parallel()

			out, err := getBuildTags(tt.in, tt.skipCSV)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, out)
		})
	}
}

func newTestMysqlDaemon(t *testing.T, port int32) *fakemysqldaemon.FakeMysqlDaemon {
	t.Helper()

	db := fakesqldb.New(t)
	db.AddQueryPattern("SET @@.*", &sqltypes.Result{})
	db.AddQueryPattern("BEGIN", &sqltypes.Result{})
	db.AddQueryPattern("COMMIT", &sqltypes.Result{})

	db.AddQueryPattern("CREATE DATABASE IF NOT EXISTS _vt", &sqltypes.Result{})
	db.AddQueryPattern("CREATE TABLE IF NOT EXISTS _vt\\.(local|shard)_metadata.*", &sqltypes.Result{})

	db.AddQueryPattern("ALTER TABLE _vt\\.local_metadata ADD COLUMN (db_name).*", &sqltypes.Result{})
	db.AddQueryPattern("ALTER TABLE _vt\\.local_metadata DROP PRIMARY KEY, ADD PRIMARY KEY\\(name, db_name\\)", &sqltypes.Result{})
	db.AddQueryPattern("ALTER TABLE _vt\\.local_metadata CHANGE value.*", &sqltypes.Result{})

	db.AddQueryPattern("ALTER TABLE _vt\\.shard_metadata ADD COLUMN (db_name).*", &sqltypes.Result{})
	db.AddQueryPattern("ALTER TABLE _vt\\.shard_metadata DROP PRIMARY KEY, ADD PRIMARY KEY\\(name, db_name\\)", &sqltypes.Result{})

	db.AddQueryPattern("UPDATE _vt\\.(local|shard)_metadata SET db_name='.+' WHERE db_name=''", &sqltypes.Result{})
	db.AddQueryPattern("INSERT INTO _vt\\.local_metadata \\(.+\\) VALUES \\(.+\\) ON DUPLICATE KEY UPDATE value ?= ?'.+'.*", &sqltypes.Result{})

	mysqld := fakemysqldaemon.NewFakeMysqlDaemon(db)
	mysqld.MysqlPort = sync2.NewAtomicInt32(port)

	return mysqld
}

func newTestTM(t *testing.T, ts *topo.Server, uid int, keyspace, shard string) *TabletManager {
	t.Helper()
	ctx := context.Background()
	tablet := newTestTablet(t, uid, keyspace, shard)
	tm := &TabletManager{
		BatchCtx:            ctx,
		TopoServer:          ts,
		MysqlDaemon:         newTestMysqlDaemon(t, 1),
		DBConfigs:           &dbconfigs.DBConfigs{},
		QueryServiceControl: tabletservermock.NewController(),
	}
	err := tm.Start(tablet, 0)
	require.NoError(t, err)

	// Wait for SrvKeyspace to be rebuilt. We know that it has been built
	// when isShardServing or tabletControls maps is non-empty.
	timeout := time.After(1 * time.Second)
	for {
		select {
		case <-timeout:
			t.Logf("servingKeyspace not initialized for tablet uid - %d", uid)
			return tm
		default:
			isNonEmpty := false
			func() {
				tm.tmState.mu.Lock()
				defer tm.tmState.mu.Unlock()
				if tm.tmState.isShardServing != nil || tm.tmState.tabletControls != nil {
					isNonEmpty = true
				}
			}()
			if isNonEmpty {
				return tm
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func newTestTablet(t *testing.T, uid int, keyspace, shard string) *topodatapb.Tablet {
	shard, keyRange, err := topo.ValidateShardName(shard)
	require.NoError(t, err)
	return &topodatapb.Tablet{
		Alias: &topodatapb.TabletAlias{
			Cell: "cell1",
			Uid:  uint32(uid),
		},
		Hostname: "localhost",
		PortMap: map[string]int32{
			"vt":   int32(1234),
			"grpc": int32(3456),
		},
		Keyspace: keyspace,
		Shard:    shard,
		KeyRange: keyRange,
		Type:     topodatapb.TabletType_REPLICA,
	}
}

func ensureSrvKeyspace(t *testing.T, ts *topo.Server, cell, keyspace string) {
	t.Helper()
	found := false
	for i := 0; i < 10; i++ {
		_, err := ts.GetSrvKeyspace(context.Background(), cell, "ks")
		if err == nil {
			found = true
			break
		}
		require.True(t, topo.IsErrType(err, topo.NoNode), err)
		time.Sleep(rebuildKeyspaceRetryInterval)
	}
	assert.True(t, found)
}
