package db_test

import (
	"context"
	"sync"
	"testing"
	"testing/fstest"

	"socialai/shared/db"
	"socialai/shared/db/dbtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateIsIdempotent(t *testing.T) {
	pool := dbtest.New(t) // already applied every migration once
	ctx := context.Background()

	fsys := fstest.MapFS{
		"0100_extra.sql": {Data: []byte(`CREATE TABLE extra (id int PRIMARY KEY);`)},
	}
	done, err := db.Migrate(ctx, pool, fsys)
	require.NoError(t, err)
	assert.Equal(t, []string{"0100_extra"}, done)

	done, err = db.Migrate(ctx, pool, fsys)
	require.NoError(t, err)
	assert.Empty(t, done, "a second run applies nothing")
}

func TestConcurrentMigratorsApplyEachFileOnce(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	// Not idempotent SQL on purpose: applying it twice would fail.
	fsys := fstest.MapFS{"0100_once.sql": {Data: []byte(`CREATE TABLE once (id int);`)}}

	var wg sync.WaitGroup
	errs := make([]error, 8)
	applied := make([][]string, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			applied[i], errs[i] = db.Migrate(ctx, pool, fsys)
		}(i)
	}
	wg.Wait()
	total := 0
	for i := range errs {
		require.NoError(t, errs[i])
		total += len(applied[i])
	}
	assert.Equal(t, 1, total, "the advisory lock serialises migrators")
}

func TestEditedMigrationIsRefused(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	_, err := db.Migrate(ctx, pool, fstest.MapFS{"0100_x.sql": {Data: []byte(`CREATE TABLE x (id int);`)}})
	require.NoError(t, err)

	_, err = db.Migrate(ctx, pool, fstest.MapFS{"0100_x.sql": {Data: []byte(`CREATE TABLE x (id bigint);`)}})
	assert.ErrorContains(t, err, "was changed after it was applied")
}

func TestFailedMigrationLeavesNoTrace(t *testing.T) {
	pool := dbtest.New(t)
	ctx := context.Background()
	bad := fstest.MapFS{"0100_bad.sql": {Data: []byte(`CREATE TABLE half (id int); SELECT no_such_function();`)}}
	_, err := db.Migrate(ctx, pool, bad)
	require.Error(t, err)

	var exists bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT to_regclass('half') IS NOT NULL`).Scan(&exists))
	assert.False(t, exists, "DDL and the version row commit together or not at all")
}
