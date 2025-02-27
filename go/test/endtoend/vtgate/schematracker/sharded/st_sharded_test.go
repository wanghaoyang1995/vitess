/*
Copyright 2021 The Vitess Authors.

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

package sharded

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	"vitess.io/vitess/go/test/endtoend/utils"

	"github.com/google/go-cmp/cmp"

	"github.com/stretchr/testify/require"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/test/endtoend/cluster"
)

var (
	clusterInstance *cluster.LocalProcessCluster
	vtParams        mysql.ConnParams
	KeyspaceName    = "ks"
	Cell            = "test"
	SchemaSQL       = `
create table t2(
	id3 bigint,
	id4 bigint,
	primary key(id3)
) Engine=InnoDB;

create table t2_id4_idx(
	id bigint not null auto_increment,
	id4 bigint,
	id3 bigint,
	primary key(id),
	key idx_id4(id4)
) Engine=InnoDB;

create table t8(
	id8 bigint,
	testId bigint,
	primary key(id8)
) Engine=InnoDB;
`

	VSchema = `
{
  "sharded": true,
  "vindexes": {
    "unicode_loose_xxhash" : {
	  "type": "unicode_loose_xxhash"
    },
    "unicode_loose_md5" : {
	  "type": "unicode_loose_md5"
    },
    "hash": {
      "type": "hash"
    },
    "xxhash": {
      "type": "xxhash"
    },
    "t2_id4_idx": {
      "type": "lookup_hash",
      "params": {
        "table": "t2_id4_idx",
        "from": "id4",
        "to": "id3",
        "autocommit": "true"
      },
      "owner": "t2"
    }
  },
  "tables": {
    "t2": {
      "column_vindexes": [
        {
          "column": "id3",
          "name": "hash"
        },
        {
          "column": "id4",
          "name": "t2_id4_idx"
        }
      ]
    },
    "t2_id4_idx": {
      "column_vindexes": [
        {
          "column": "id4",
          "name": "hash"
        }
      ]
    },
    "t8": {
      "column_vindexes": [
        {
          "column": "id8",
          "name": "hash"
        }
      ]
    }
  }
}`
)

func TestMain(m *testing.M) {
	defer cluster.PanicHandler(nil)
	flag.Parse()

	exitCode := func() int {
		clusterInstance = cluster.NewCluster(Cell, "localhost")
		defer clusterInstance.Teardown()

		// Start topo server
		err := clusterInstance.StartTopo()
		if err != nil {
			return 1
		}

		// Start keyspace
		keyspace := &cluster.Keyspace{
			Name:      KeyspaceName,
			SchemaSQL: SchemaSQL,
			VSchema:   VSchema,
		}
		clusterInstance.VtGateExtraArgs = []string{"--schema_change_signal", "--vschema_ddl_authorized_users", "%", "--schema_change_signal_user", "userData1"}
		clusterInstance.VtTabletExtraArgs = []string{"--queryserver-config-schema-change-signal", "--queryserver-config-schema-change-signal-interval", "0.1", "--queryserver-config-strict-table-acl", "--queryserver-config-acl-exempt-acl", "userData1", "--table-acl-config", "dummy.json"}
		err = clusterInstance.StartKeyspace(*keyspace, []string{"-80", "80-"}, 1, true)
		if err != nil {
			return 1
		}

		// Start vtgate
		err = clusterInstance.StartVtgate()
		if err != nil {
			return 1
		}

		vtParams = mysql.ConnParams{
			Host: clusterInstance.Hostname,
			Port: clusterInstance.VtgateMySQLPort,
		}
		return m.Run()
	}()
	os.Exit(exitCode)
}

func TestNewTable(t *testing.T) {
	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &vtParams)
	require.NoError(t, err)
	defer conn.Close()

	shard1Params := vtParams
	shard1Params.DbName += ":-80@primary"
	connShard1, err := mysql.Connect(ctx, &shard1Params)
	require.NoError(t, err)
	defer connShard1.Close()

	shard2Params := vtParams
	shard2Params.DbName += ":80-@primary"
	connShard2, err := mysql.Connect(ctx, &shard2Params)
	require.NoError(t, err)
	defer connShard2.Close()

	_ = utils.Exec(t, conn, "create table test_table (id bigint, name varchar(100))")

	time.Sleep(2 * time.Second)

	utils.AssertMatches(t, conn, "select * from test_table", `[]`)
	utils.AssertMatches(t, connShard1, "select * from test_table", `[]`)
	utils.AssertMatches(t, connShard2, "select * from test_table", `[]`)

	utils.Exec(t, conn, "drop table test_table")

	time.Sleep(2 * time.Second)
}

func TestAmbiguousColumnJoin(t *testing.T) {
	defer cluster.PanicHandler(t)
	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &vtParams)
	require.NoError(t, err)
	defer conn.Close()
	// this query only works if we know which table the testId belongs to. The vschema does not contain
	// this info, so we are testing that the schema tracker has added column info to the vschema
	_, err = conn.ExecuteFetch(`select testId from t8 join t2`, 1000, true)
	require.NoError(t, err)
}

func TestInitAndUpdate(t *testing.T) {
	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &vtParams)
	require.NoError(t, err)
	defer conn.Close()

	utils.AssertMatches(t, conn, "SHOW VSCHEMA TABLES", `[[VARCHAR("dual")] [VARCHAR("t2")] [VARCHAR("t2_id4_idx")] [VARCHAR("t8")]]`)

	// Init
	_ = utils.Exec(t, conn, "create table test_sc (id bigint primary key)")
	assertMatchesWithTimeout(t, conn,
		"SHOW VSCHEMA TABLES",
		`[[VARCHAR("dual")] [VARCHAR("t2")] [VARCHAR("t2_id4_idx")] [VARCHAR("t8")] [VARCHAR("test_sc")]]`,
		100*time.Millisecond,
		3*time.Second,
		"test_sc not in vschema tables")

	// Tables Update via health check.
	_ = utils.Exec(t, conn, "create table test_sc1 (id bigint primary key)")
	assertMatchesWithTimeout(t, conn,
		"SHOW VSCHEMA TABLES",
		`[[VARCHAR("dual")] [VARCHAR("t2")] [VARCHAR("t2_id4_idx")] [VARCHAR("t8")] [VARCHAR("test_sc")] [VARCHAR("test_sc1")]]`,
		100*time.Millisecond,
		3*time.Second,
		"test_sc1 not in vschema tables")

	_ = utils.Exec(t, conn, "drop table test_sc, test_sc1")
	assertMatchesWithTimeout(t, conn,
		"SHOW VSCHEMA TABLES",
		`[[VARCHAR("dual")] [VARCHAR("t2")] [VARCHAR("t2_id4_idx")] [VARCHAR("t8")]]`,
		100*time.Millisecond,
		3*time.Second,
		"test_sc and test_sc_1 should not be in vschema tables")

}

func TestDMLOnNewTable(t *testing.T) {
	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &vtParams)
	require.NoError(t, err)
	defer conn.Close()

	// create a new table which is not part of the VSchema
	utils.Exec(t, conn, `create table new_table_tracked(id bigint, name varchar(100), primary key(id)) Engine=InnoDB`)

	// wait for vttablet's schema reload interval to pass
	assertMatchesWithTimeout(t, conn,
		"SHOW VSCHEMA TABLES",
		`[[VARCHAR("dual")] [VARCHAR("new_table_tracked")] [VARCHAR("t2")] [VARCHAR("t2_id4_idx")] [VARCHAR("t8")]]`,
		100*time.Millisecond,
		3*time.Second,
		"test_sc not in vschema tables")

	utils.AssertMatches(t, conn, "select id from new_table_tracked", `[]`)              // select
	utils.AssertMatches(t, conn, "select id from new_table_tracked where id = 5", `[]`) // select
	// DML on new table
	// insert initial data ,update and delete will fail since we have not added a primary vindex
	errorMessage := "table 'new_table_tracked' does not have a primary vindex (errno 1173) (sqlstate 42000)"
	utils.AssertContainsError(t, conn, `insert into new_table_tracked(id) values(0),(1)`, errorMessage)
	utils.AssertContainsError(t, conn, `update new_table_tracked set name = "newName1"`, errorMessage)
	utils.AssertContainsError(t, conn, "delete from new_table_tracked", errorMessage)

	utils.Exec(t, conn, `select name from new_table_tracked join t8`)

	// add a primary vindex for the table
	utils.Exec(t, conn, "alter vschema on ks.new_table_tracked add vindex hash(id) using hash")
	time.Sleep(1 * time.Second)
	utils.Exec(t, conn, `insert into new_table_tracked(id) values(0),(1)`)
	utils.Exec(t, conn, `insert into t8(id8) values(2)`)
	defer utils.Exec(t, conn, `delete from t8`)
	utils.AssertMatchesNoOrder(t, conn, `select id from new_table_tracked join t8`, `[[INT64(0)] [INT64(1)]]`)
}

func assertMatchesWithTimeout(t *testing.T, conn *mysql.Conn, query, expected string, r time.Duration, d time.Duration, failureMsg string) {
	t.Helper()
	timeout := time.After(d)
	diff := "actual and expectation does not match"
	for len(diff) > 0 {
		select {
		case <-timeout:
			require.Fail(t, failureMsg, diff)
		case <-time.After(r):
			qr := utils.Exec(t, conn, query)
			diff = cmp.Diff(expected,
				fmt.Sprintf("%v", qr.Rows))
		}

	}
}
