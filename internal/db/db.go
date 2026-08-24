package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	_ "embed"
	"fmt"
	"math/bits"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
)

// basenameSQL is the body of the SQLite `basename(path)` scalar
// function. Returns the substring of `path` after the last `/` or `\`:
// canonical_path holds native absolute paths, so a library written on
// Windows carries backslashes. NULL passes through as NULL. Used by
// the search executor's `name:` filter so the match can target the
// filename segment without bleeding into folder names; pure SQLite
// has no built-in for this since `reverse()` isn't part of the
// modernc build, so registering the function is the cleanest path
// that avoids a denormalised `basename` column.
func basenameSQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 1 || args[0] == nil {
		return nil, nil
	}
	s, ok := args[0].(string)
	if !ok {
		return nil, nil
	}
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		return s[i+1:], nil
	}
	return s, nil
}

// hammingDistSQL is the body of the SQLite `hammingdist(int64, int64)`
// scalar function: Hamming distance between the two 64-bit values
// interpreted as unsigned bit patterns. SQLite has no XOR operator on
// integers (^ is unsupported in the modernc dialect), so the search
// executor calls this when the per-gallery BK-tree isn't wired and
// it needs to compute distance in pure SQL for the `phash:<hex>~d`
// filter. NULL on either side passes through as NULL so a phashless
// row drops out of the comparison.
func hammingDistSQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 2 || args[0] == nil || args[1] == nil {
		return nil, nil
	}
	a, aOk := args[0].(int64)
	b, bOk := args[1].(int64)
	if !aOk || !bOk {
		return nil, nil
	}
	return int64(bits.OnesCount64(uint64(a) ^ uint64(b))), nil
}

// randomKeySQL is the body of the SQLite `random_key(image_id, seed)`
// scalar function. Maps (id, seed) to a deterministic 63-bit value via
// a SplitMix64-style mix so the executor's random-sort ORDER BY
// produces a uniformly scattered permutation even for small seeds.
//
// NULL on either side falls through as NULL so SQLite's NULL-ordering
// rules apply consistently with the (id, key) cursor comparison.
func randomKeySQL(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
	if len(args) != 2 || args[0] == nil || args[1] == nil {
		return nil, nil
	}
	id, aOk := args[0].(int64)
	seed, bOk := args[1].(int64)
	if !aOk || !bOk {
		return nil, nil
	}
	return int64(RandomSortKey(id, seed)), nil
}

// RandomSortKey computes the same 63-bit key SQLite's random_key()
// emits, so Go-side callers (cursor seek in ExecuteAdjacent, rank
// computation in RankInQuery) can produce the matching keyVal without
// a round trip. Stable across calls; depends only on (id, seed).
func RandomSortKey(id, seed int64) uint64 {
	x := uint64(id)*0x9E3779B97F4A7C15 ^ uint64(seed)*0xBF58476D1CE4E5B9
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return x & 0x7FFFFFFFFFFFFFFF
}

func init() {
	// Registered once for the driver; available on every connection
	// opened afterwards.
	sqlite.MustRegisterDeterministicScalarFunction("basename", 1, basenameSQL)
	sqlite.MustRegisterDeterministicScalarFunction("hammingdist", 2, hammingDistSQL)
	sqlite.MustRegisterDeterministicScalarFunction("random_key", 2, randomKeySQL)
}

//go:embed schema.sql
var schemaSQL string

// NormalizeWindowsFolderPathSQL rewrites folder_path from the native
// separators a Windows build stored to the "/"-separated form every
// other platform reads. The row has to prove it came from one - a
// backslash-separated canonical_path with no "/" anywhere - so a POSIX
// directory with a backslash in its name stays untouched. Run by
// Bootstrap on the schema-marker gap and by the gallery import, which
// builds its DB at the current marker and so never sees that gap.
const NormalizeWindowsFolderPathSQL = `UPDATE images SET folder_path = ltrim(replace(folder_path, '\', '/'), '/')
	WHERE instr(folder_path, '\') > 0
	  AND instr(canonical_path, '\') > 0
	  AND instr(canonical_path, '/') = 0`

// bootstrapSchemaVersion is the marker Bootstrap stores in
// PRAGMA user_version once it has applied every migration in this file
// and refreshed sqlite_stat1. Bump it when a migration adds a column or
// index the planner needs stats for; Bootstrap then runs ANALYZE on the
// next boot after the upgrade and skips it on every boot afterwards.
const bootstrapSchemaVersion = 13

// DB holds read and write connection pools for the SQLite database.
// WAL mode allows concurrent readers but serialises writers, so the read
// pool has many connections and the write pool has one.
type DB struct {
	Read  *sql.DB
	Write *sql.DB
}

// Open opens both connection pools pointing at the same SQLite file.
func Open(path string) (*DB, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(on)" +
		"&_pragma=journal_mode(wal)" +
		"&_pragma=synchronous(normal)" +
		"&_pragma=cache_size(-1024)" +
		"&_pragma=temp_store(memory)" +
		"&_pragma=mmap_size(67108864)"

	rd, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening read pool: %w", err)
	}
	rd.SetMaxOpenConns(8)
	rd.SetMaxIdleConns(8)
	rd.SetConnMaxIdleTime(5 * time.Minute)

	wr, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = rd.Close()
		return nil, fmt.Errorf("opening write pool: %w", err)
	}
	wr.SetMaxOpenConns(1)
	wr.SetMaxIdleConns(1)
	wr.SetConnMaxIdleTime(5 * time.Minute)

	db := &DB{Read: rd, Write: wr}

	if err := rd.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging read pool: %w", err)
	}
	if err := wr.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging write pool: %w", err)
	}

	return db, nil
}

// ShrinkMemory runs `PRAGMA shrink_memory` on every connection in
// both pools, returning freed pages from modernc/sqlite's caches to
// the kernel. Each connection has its own page cache, so all are
// reserved up front to guarantee coverage.
func (db *DB) ShrinkMemory(ctx context.Context) error {
	if err := shrinkPool(ctx, db.Read); err != nil {
		return err
	}
	return shrinkPool(ctx, db.Write)
}

func shrinkPool(ctx context.Context, pool *sql.DB) error {
	n := pool.Stats().MaxOpenConnections
	if n <= 0 {
		n = 1
	}
	conns := make([]*sql.Conn, 0, n)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for i := 0; i < n; i++ {
		c, err := pool.Conn(ctx)
		if err != nil {
			return err
		}
		conns = append(conns, c)
	}
	for _, c := range conns {
		if _, err := c.ExecContext(ctx, `PRAGMA shrink_memory`); err != nil {
			return err
		}
	}
	return nil
}

// Close closes both connection pools.
func (db *DB) Close() error {
	var firstErr error
	if err := db.Read.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := db.Write.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
