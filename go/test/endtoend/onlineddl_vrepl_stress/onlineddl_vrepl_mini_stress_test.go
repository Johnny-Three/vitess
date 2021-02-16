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

package onlineddl

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"vitess.io/vitess/go/mysql"
	"vitess.io/vitess/go/sqltypes"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/schema"

	"vitess.io/vitess/go/test/endtoend/cluster"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type WriteMetrics struct {
	mu                                                      sync.Mutex
	insertsAttempts, insertsFailures, insertsNoops, inserts int64
	updatesAttempts, updatesFailures, updatesNoops, updates int64
	deletesAttempts, deletesFailures, deletesNoops, deletes int64
}

func (w *WriteMetrics) Clear() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.inserts = 0
	w.updates = 0
	w.deletes = 0

	w.insertsAttempts = 0
	w.insertsFailures = 0
	w.insertsNoops = 0

	w.updatesAttempts = 0
	w.updatesFailures = 0
	w.updatesNoops = 0

	w.deletesAttempts = 0
	w.deletesFailures = 0
	w.deletesNoops = 0
}

func (w *WriteMetrics) String() string {
	return fmt.Sprintf(`WriteMetrics: inserts-deletes=%d, updates-deletes=%d,
insertsAttempts=%d, insertsFailures=%d, insertsNoops=%d, inserts=%d,
updatesAttempts=%d, updatesFailures=%d, updatesNoops=%d, updates=%d,
deletesAttempts=%d, deletesFailures=%d, deletesNoops=%d, deletes=%d,
`,
		w.inserts-w.deletes, w.updates-w.deletes,
		w.insertsAttempts, w.insertsFailures, w.insertsNoops, w.inserts,
		w.updatesAttempts, w.updatesFailures, w.updatesNoops, w.updates,
		w.deletesAttempts, w.deletesFailures, w.deletesNoops, w.deletes,
	)
}

var (
	clusterInstance *cluster.LocalProcessCluster
	vtParams        mysql.ConnParams

	hostname              = "localhost"
	keyspaceName          = "ks"
	cell                  = "zone1"
	schemaChangeDirectory = ""
	tableName             = `stress_test`
	createStatement       = `
		CREATE TABLE stress_test (
			id bigint(20) not null,
			rand_val varchar(32) null default '',
			hint_col varchar(64) not null default '',
			created_timestamp timestamp not null default current_timestamp,
			updates int unsigned not null default 0,
			PRIMARY KEY (id),
			key created_idx(created_timestamp),
			key updates_idx(updates)
		) ENGINE=InnoDB
	`
	alterHintStatement = `
		ALTER TABLE stress_test modify hint_col varchar(64) not null default '%s'
	`
	insertRowStatement = `
		INSERT IGNORE INTO stress_test (id, rand_val) VALUES (%d, left(md5(rand()), 8))
	`
	updateRowStatement = `
		UPDATE stress_test SET updates=updates+1 WHERE id=%d
	`
	deleteRowStatement = `
		DELETE FROM stress_test WHERE id=%d AND updates=1
	`
	// We use CAST(SUM(updates) AS SIGNED) because SUM() returns a DECIMAL datatype, and we want to read a SIGNED INTEGER type
	selectCountRowsStatement = `
		SELECT COUNT(*) AS num_rows, CAST(SUM(updates) AS SIGNED) AS sum_updates FROM stress_test
	`
	truncateStatement = `
		TRUNCATE TABLE stress_test
	`
	writeMetrics WriteMetrics
)

const (
	maxTableRows    = 4096
	maxConcurrency  = 5
	countIterations = 5
)

func fullWordUUIDRegexp(uuid, searchWord string) *regexp.Regexp {
	return regexp.MustCompile(uuid + `.*?\b` + searchWord + `\b`)
}

func TestMain(m *testing.M) {
	defer cluster.PanicHandler(nil)
	flag.Parse()

	exitcode, err := func() (int, error) {
		clusterInstance = cluster.NewCluster(cell, hostname)
		schemaChangeDirectory = path.Join("/tmp", fmt.Sprintf("schema_change_dir_%d", clusterInstance.GetAndReserveTabletUID()))
		defer os.RemoveAll(schemaChangeDirectory)
		defer clusterInstance.Teardown()

		if _, err := os.Stat(schemaChangeDirectory); os.IsNotExist(err) {
			_ = os.Mkdir(schemaChangeDirectory, 0700)
		}

		clusterInstance.VtctldExtraArgs = []string{
			"-schema_change_dir", schemaChangeDirectory,
			"-schema_change_controller", "local",
			"-schema_change_check_interval", "1"}

		clusterInstance.VtTabletExtraArgs = []string{
			"-enable-lag-throttler",
			"-throttle_threshold", "1s",
			"-heartbeat_enable",
			"-heartbeat_interval", "250ms",
			"-migration_check_interval", "5s",
		}
		clusterInstance.VtGateExtraArgs = []string{
			"-ddl_strategy", "online",
		}

		if err := clusterInstance.StartTopo(); err != nil {
			return 1, err
		}

		// Start keyspace
		keyspace := &cluster.Keyspace{
			Name: keyspaceName,
		}

		if err := clusterInstance.StartUnshardedKeyspace(*keyspace, 0, false); err != nil {
			return 1, err
		}
		if err := clusterInstance.StartKeyspace(*keyspace, []string{"1"}, 1, false); err != nil {
			return 1, err
		}

		vtgateInstance := clusterInstance.NewVtgateInstance()
		// set the gateway we want to use
		vtgateInstance.GatewayImplementation = "tabletgateway"
		// Start vtgate
		if err := vtgateInstance.Setup(); err != nil {
			return 1, err
		}
		// ensure it is torn down during cluster TearDown
		clusterInstance.VtgateProcess = *vtgateInstance
		vtParams = mysql.ConnParams{
			Host: clusterInstance.Hostname,
			Port: clusterInstance.VtgateMySQLPort,
		}

		return m.Run(), nil
	}()
	if err != nil {
		fmt.Printf("%v\n", err)
		os.Exit(1)
	} else {
		os.Exit(exitcode)
	}

}

func TestSchemaChange(t *testing.T) {
	defer cluster.PanicHandler(t)

	t.Run("create schema", func(t *testing.T) {
		assert.Equal(t, 2, len(clusterInstance.Keyspaces[0].Shards))
		testWithInitialSchema(t)
	})
	for i := 0; i < countIterations; i++ {
		testName := fmt.Sprintf("workload without ALTER TABLE %d/%d", (i + 1), countIterations)
		t.Run(testName, func(t *testing.T) {
			initTable(t)
			testSelectTableMetrics(t)
		})
	}
	t.Run("ALTER TABLE without workload", func(t *testing.T) {
		initTable(t)
		hint := "hint-alter-without-workload"
		uuid := testOnlineDDLStatement(t, fmt.Sprintf(alterHintStatement, hint), "online", "vtgate", hint)
		checkRecentMigrations(t, uuid, schema.OnlineDDLStatusComplete)
		testSelectTableMetrics(t)
	})

	ctx := context.Background()
	for i := 0; i < countIterations; i++ {
		testName := fmt.Sprintf("ALTER TABLE with workload %d/%d", (i + 1), countIterations)
		t.Run(testName, func(t *testing.T) {
			initTable(t)
			done := make(chan bool)
			go runMultipleConnections(ctx, t, done)
			hint := fmt.Sprintf("hint-alter-with-workload-%d", i)
			uuid := testOnlineDDLStatement(t, fmt.Sprintf(alterHintStatement, hint), "online", "vtgate", hint)
			checkRecentMigrations(t, uuid, schema.OnlineDDLStatusComplete)
			done <- true
			testSelectTableMetrics(t)
		})
	}
}

func testWithInitialSchema(t *testing.T) {
	// Create the stress table
	err := clusterInstance.VtctlclientProcess.ApplySchema(keyspaceName, createStatement)
	require.Nil(t, err)

	// Check if table is created
	checkTable(t, tableName)
}

// testOnlineDDLStatement runs an online DDL, ALTER statement
func testOnlineDDLStatement(t *testing.T, alterStatement string, ddlStrategy string, executeStrategy string, expectHint string) (uuid string) {
	if executeStrategy == "vtgate" {
		row := vtgateExec(t, ddlStrategy, alterStatement, "").Named().Row()
		if row != nil {
			uuid = row.AsString("uuid", "")
		}
	} else {
		var err error
		uuid, err = clusterInstance.VtctlclientProcess.ApplySchemaWithOutput(keyspaceName, alterStatement, ddlStrategy)
		assert.NoError(t, err)
	}
	uuid = strings.TrimSpace(uuid)
	fmt.Println("# Generated UUID (for debug purposes):")
	fmt.Printf("<%s>\n", uuid)

	strategy, _, err := schema.ParseDDLStrategy(ddlStrategy)
	assert.NoError(t, err)

	if !strategy.IsDirect() {
		time.Sleep(time.Second * 20)
	}

	if expectHint != "" {
		checkMigratedTable(t, tableName, expectHint)
	}
	return uuid
}

// checkTable checks the number of tables in the first two shards.
func checkTable(t *testing.T, showTableName string) {
	for i := range clusterInstance.Keyspaces[0].Shards {
		checkTablesCount(t, clusterInstance.Keyspaces[0].Shards[i].Vttablets[0], showTableName, 1)
	}
}

// checkTablesCount checks the number of tables in the given tablet
func checkTablesCount(t *testing.T, tablet *cluster.Vttablet, showTableName string, expectCount int) {
	query := fmt.Sprintf(`show tables like '%%%s%%';`, showTableName)
	queryResult, err := tablet.VttabletProcess.QueryTablet(query, keyspaceName, true)
	require.Nil(t, err)
	assert.Equal(t, expectCount, len(queryResult.Rows))
}

// checkRecentMigrations checks 'OnlineDDL <keyspace> show recent' output. Example to such output:
// +------------------+-------+--------------+-------------+--------------------------------------+----------+---------------------+---------------------+------------------+
// |      Tablet      | shard | mysql_schema | mysql_table |            migration_uuid            | strategy |  started_timestamp  | completed_timestamp | migration_status |
// +------------------+-------+--------------+-------------+--------------------------------------+----------+---------------------+---------------------+------------------+
// | zone1-0000003880 |     0 | vt_ks        | stress_test | a0638f6b_ec7b_11ea_9bf8_000d3a9b8a9a | online   | 2020-09-01 17:50:40 | 2020-09-01 17:50:41 | complete         |
// | zone1-0000003884 |     1 | vt_ks        | stress_test | a0638f6b_ec7b_11ea_9bf8_000d3a9b8a9a | online   | 2020-09-01 17:50:40 | 2020-09-01 17:50:41 | complete         |
// +------------------+-------+--------------+-------------+--------------------------------------+----------+---------------------+---------------------+------------------+

func checkRecentMigrations(t *testing.T, uuid string, expectStatus schema.OnlineDDLStatus) {
	result, err := clusterInstance.VtctlclientProcess.OnlineDDLShowRecent(keyspaceName)
	assert.NoError(t, err)
	fmt.Println("# 'vtctlclient OnlineDDL show recent' output (for debug purposes):")
	fmt.Println(result)
	assert.Equal(t, len(clusterInstance.Keyspaces[0].Shards), strings.Count(result, uuid))
	// We ensure "full word" regexp becuase some column names may conflict
	expectStatusRegexp := fullWordUUIDRegexp(uuid, string(expectStatus))
	m := expectStatusRegexp.FindAllString(result, -1)
	assert.Equal(t, len(clusterInstance.Keyspaces[0].Shards), len(m))

	result, err = clusterInstance.VtctlclientProcess.VExec(keyspaceName, uuid, `select migration_status, message from _vt.schema_migrations`)
	assert.NoError(t, err)
	fmt.Println("# 'vtctlclient VExec' output (for debug purposes):")
	fmt.Println(result)
}

// checkMigratedTables checks the CREATE STATEMENT of a table after migration
func checkMigratedTable(t *testing.T, tableName, expectHint string) {
	for i := range clusterInstance.Keyspaces[0].Shards {
		createStatement := getCreateTableStatement(t, clusterInstance.Keyspaces[0].Shards[i].Vttablets[0], tableName)
		assert.Contains(t, createStatement, expectHint)
	}
}

// getCreateTableStatement returns the CREATE TABLE statement for a given table
func getCreateTableStatement(t *testing.T, tablet *cluster.Vttablet, tableName string) (statement string) {
	queryResult, err := tablet.VttabletProcess.QueryTablet(fmt.Sprintf("show create table %s;", tableName), keyspaceName, true)
	require.Nil(t, err)

	assert.Equal(t, len(queryResult.Rows), 1)
	assert.Equal(t, len(queryResult.Rows[0]), 2) // table name, create statement
	statement = queryResult.Rows[0][1].ToString()
	return statement
}

func generateInsert(t *testing.T, conn *mysql.Conn, wg *sync.WaitGroup) error {
	wg.Add(1)
	id := rand.Int31n(int32(maxTableRows))
	query := fmt.Sprintf(insertRowStatement, id)
	qr, err := conn.ExecuteFetch(query, 1000, true)

	go func() {
		defer wg.Done()
		writeMetrics.mu.Lock()
		defer writeMetrics.mu.Unlock()

		writeMetrics.insertsAttempts++
		if err != nil {
			writeMetrics.insertsFailures++
			return
		}
		assert.Less(t, qr.RowsAffected, uint64(2))
		if qr.RowsAffected == 0 {
			writeMetrics.insertsNoops++
			return
		}
		writeMetrics.inserts++
	}()
	return err
}

func generateUpdate(t *testing.T, conn *mysql.Conn, wg *sync.WaitGroup) error {
	wg.Add(1)
	id := rand.Int31n(int32(maxTableRows))
	query := fmt.Sprintf(updateRowStatement, id)
	qr, err := conn.ExecuteFetch(query, 1000, true)

	go func() {
		defer wg.Done()
		writeMetrics.mu.Lock()
		defer writeMetrics.mu.Unlock()

		writeMetrics.updatesAttempts++
		if err != nil {
			writeMetrics.updatesFailures++
			return
		}
		assert.Less(t, qr.RowsAffected, uint64(2))
		if qr.RowsAffected == 0 {
			writeMetrics.updatesNoops++
			return
		}
		writeMetrics.updates++
	}()
	return err
}

func generateDelete(t *testing.T, conn *mysql.Conn, wg *sync.WaitGroup) error {
	wg.Add(1)
	id := rand.Int31n(int32(maxTableRows))
	query := fmt.Sprintf(deleteRowStatement, id)
	qr, err := conn.ExecuteFetch(query, 1000, true)

	go func() {
		defer wg.Done()
		writeMetrics.mu.Lock()
		defer writeMetrics.mu.Unlock()

		writeMetrics.deletesAttempts++
		if err != nil {
			writeMetrics.deletesFailures++
			return
		}
		assert.Less(t, qr.RowsAffected, uint64(2))
		if qr.RowsAffected == 0 {
			writeMetrics.deletesNoops++
			return
		}
		writeMetrics.deletes++
	}()
	return err
}

func runSingleConnection(ctx context.Context, t *testing.T, done chan bool, wg *sync.WaitGroup) {
	log.Infof("Running single connection")
	conn, err := mysql.Connect(ctx, &vtParams)
	require.Nil(t, err)
	defer conn.Close()

	_, err = conn.ExecuteFetch("set autocommit=1", 1000, true)
	require.Nil(t, err)
	_, err = conn.ExecuteFetch("set transaction isolation level read committed", 1000, true)
	require.Nil(t, err)

	for {
		select {
		case <-done:
			log.Infof("Terminating single connection")
			return
		default:
		}
		switch rand.Int31n(3) {
		case 0:
			err = generateInsert(t, conn, wg)
		case 1:
			err = generateUpdate(t, conn, wg)
		case 2:
			err = generateDelete(t, conn, wg)
		}
		if err != nil {
			if strings.Contains(err.Error(), "disallowed due to rule: enforce blacklisted tables") {
				err = nil
			}
		}
		assert.Nil(t, err)
		time.Sleep(10 * time.Millisecond)
	}
}

func runMultipleConnections(ctx context.Context, t *testing.T, done chan bool) {
	log.Infof("Running multiple connections")
	var chans []chan bool
	var wg sync.WaitGroup
	for i := 0; i < maxConcurrency; i++ {
		d := make(chan bool)
		chans = append(chans, d)
		go runSingleConnection(ctx, t, d, &wg)
	}
	<-done
	log.Infof("Running multiple connections: done")
	for _, d := range chans {
		log.Infof("Cancelling single connection")
		d <- true
	}
	wg.Wait()
	log.Infof("All connections cancelled")
}

func initTable(t *testing.T) {
	log.Infof("initTable begin")
	defer log.Infof("initTable complete")

	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &vtParams)
	require.Nil(t, err)
	defer conn.Close()

	writeMetrics.Clear()
	_, err = conn.ExecuteFetch(truncateStatement, 1000, true)
	require.Nil(t, err)

	var wg sync.WaitGroup
	for i := 0; i < maxTableRows/2; i++ {
		generateInsert(t, conn, &wg)
	}
	for i := 0; i < maxTableRows/4; i++ {
		generateUpdate(t, conn, &wg)
	}
	for i := 0; i < maxTableRows/4; i++ {
		generateDelete(t, conn, &wg)
	}
	wg.Wait()
}

func testSelectTableMetrics(t *testing.T) {
	writeMetrics.mu.Lock()
	defer writeMetrics.mu.Unlock()

	log.Infof("%s", writeMetrics.String())

	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &vtParams)
	require.Nil(t, err)
	defer conn.Close()

	rs, err := conn.ExecuteFetch(selectCountRowsStatement, 1000, true)
	require.Nil(t, err)

	row := rs.Named().Row()
	require.NotNil(t, row)
	log.Infof("testSelectTableMetrics, row: %v", row)
	numRows := row.AsInt64("num_rows", 0)
	sumUpdates := row.AsInt64("sum_updates", 0)

	assert.NotZero(t, numRows)
	assert.NotZero(t, sumUpdates)
	assert.NotZero(t, writeMetrics.inserts)
	assert.NotZero(t, writeMetrics.deletes)
	assert.NotZero(t, writeMetrics.updates)
	assert.Equal(t, writeMetrics.inserts-writeMetrics.deletes, numRows)
	assert.Equal(t, writeMetrics.updates-writeMetrics.deletes, sumUpdates) // because we DELETE WHERE updates=1
}

func vtgateExec(t *testing.T, ddlStrategy string, query string, expectError string) *sqltypes.Result {
	t.Helper()

	ctx := context.Background()
	conn, err := mysql.Connect(ctx, &vtParams)
	require.Nil(t, err)
	defer conn.Close()

	setSession := fmt.Sprintf("set @@ddl_strategy='%s'", ddlStrategy)
	_, err = conn.ExecuteFetch(setSession, 1000, true)
	assert.NoError(t, err)

	qr, err := conn.ExecuteFetch(query, 1000, true)
	if expectError == "" {
		require.NoError(t, err)
	} else {
		require.Error(t, err, "error should not be nil")
		assert.Contains(t, err.Error(), expectError, "Unexpected error")
	}
	return qr
}
