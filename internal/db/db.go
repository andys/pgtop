package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Conn wraps a pgx connection.
type Conn = *pgx.Conn

// Process represents a running PostgreSQL backend process.
type Process struct {
	PID          int
	Query        string
	State        string
	User         string
	Database     string
	XactSeconds  int64 // seconds since transaction start
	QuerySeconds int64 // seconds since query start
	Locks        int
}

// Stats holds database-level statistics.
type Stats struct {
	DatabaseName  string
	QueriesPerSec float64
	TotalConns    int
	ActiveConns   int
	XactCommit    int64
	XactRollback  int64
	BlksRead      int64
	BlksHit       int64
	TupReturned   int64
	TupFetched    int64
}

// Lock represents a lock held by a process.
type Lock struct {
	Database string
	Schema   string
	Table    string
	Index    string
	Mode     string
	Granted  bool
}

// Connect establishes a connection to PostgreSQL using the given URI.
func Connect(ctx context.Context, uri string) (Conn, error) {
	conn, err := pgx.Connect(ctx, uri)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return conn, nil
}

// GetVersion returns the PostgreSQL server version string.
func GetVersion(ctx context.Context, conn Conn) (string, error) {
	var version string
	err := conn.QueryRow(ctx, "SHOW server_version").Scan(&version)
	if err != nil {
		return "", err
	}
	return version, nil
}

// GetProcesses returns all PostgreSQL backend processes with lock counts.
const queryProcesses = `
WITH lock_activity AS (
    SELECT pid, count(*) AS lock_count
    FROM pg_locks
    WHERE relation IS NOT NULL
    GROUP BY pid
)
SELECT a.pid,
       COALESCE(a.query, '') AS query,
       COALESCE(a.state, '') AS state,
       COALESCE(a.usename, '') AS usename,
       COALESCE(a.datname, '') AS datname,
       COALESCE(extract(EPOCH FROM age(clock_timestamp(), a.xact_start))::BIGINT, 0) AS xact_secs,
       COALESCE(extract(EPOCH FROM age(clock_timestamp(), a.query_start))::BIGINT, 0) AS query_secs,
       COALESCE(b.lock_count, 0) AS lock_count
FROM pg_stat_activity a
LEFT OUTER JOIN lock_activity b ON a.pid = b.pid
WHERE a.pid != pg_backend_pid()
ORDER BY query_secs DESC;
`

func GetProcesses(ctx context.Context, conn Conn) ([]Process, error) {
	rows, err := conn.Query(ctx, queryProcesses)
	if err != nil {
		return nil, fmt.Errorf("query processes: %w", err)
	}
	defer rows.Close()

	var procs []Process
	for rows.Next() {
		var p Process
		err := rows.Scan(&p.PID, &p.Query, &p.State, &p.User, &p.Database,
			&p.XactSeconds, &p.QuerySeconds, &p.Locks)
		if err != nil {
			return nil, fmt.Errorf("scan process: %w", err)
		}
		procs = append(procs, p)
	}
	return procs, rows.Err()
}

// GetStats returns database-level statistics from pg_stat_database.
const queryStats = `
SELECT COALESCE(datname, ''),
       COALESCE(xact_commit, 0),
       COALESCE(xact_rollback, 0),
       COALESCE(blks_read, 0),
       COALESCE(blks_hit, 0),
       COALESCE(tup_returned, 0),
       COALESCE(tup_fetched, 0)
FROM pg_stat_database
WHERE datname = current_database();
`

func GetStats(ctx context.Context, conn Conn) (Stats, error) {
	var s Stats
	err := conn.QueryRow(ctx, queryStats).Scan(
		&s.DatabaseName,
		&s.XactCommit,
		&s.XactRollback,
		&s.BlksRead,
		&s.BlksHit,
		&s.TupReturned,
		&s.TupFetched,
	)
	if err != nil {
		return Stats{}, fmt.Errorf("query stats: %w", err)
	}
	return s, nil
}

// Explain runs EXPLAIN on the given query and returns the plan text.
func Explain(ctx context.Context, conn Conn, query string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "No query to explain.", nil
	}

	// Wrap in a transaction and rollback to avoid side effects
	tx, err := conn.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	explainSQL := "EXPLAIN " + query
	rows, err := tx.Query(ctx, explainSQL)
	if err != nil {
		return fmt.Sprintf("EXPLAIN error: %v", err), nil
	}
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return fmt.Sprintf("EXPLAIN error: %v", err), nil
	}

	return strings.Join(lines, "\n"), nil
}

// GetLocks returns the locks held by a specific PID.
const queryLocks = `
SELECT COALESCE(d.datname, '') AS database,
       COALESCE(nsp.nspname, '') AS schema,
       COALESCE(r.relname, '') AS table_name,
       COALESCE(i.relname, '') AS index_name,
       l.mode,
       l.granted
FROM pg_locks l
JOIN pg_stat_activity a ON a.pid = l.pid
LEFT JOIN pg_class r ON l.relation = r.oid AND r.relkind = 'r'
LEFT JOIN pg_class i ON l.relation = i.oid AND i.relkind = 'i'
LEFT JOIN pg_namespace nsp ON COALESCE(r.relnamespace, i.relnamespace) = nsp.oid
LEFT JOIN pg_database d ON l.database = d.oid
WHERE l.pid = $1
  AND l.relation IS NOT NULL;
`

func GetLocks(ctx context.Context, conn Conn, pid int) ([]Lock, error) {
	rows, err := conn.Query(ctx, queryLocks, pid)
	if err != nil {
		return nil, fmt.Errorf("query locks: %w", err)
	}
	defer rows.Close()

	var locks []Lock
	for rows.Next() {
		var l Lock
		err := rows.Scan(&l.Database, &l.Schema, &l.Table, &l.Index, &l.Mode, &l.Granted)
		if err != nil {
			return nil, fmt.Errorf("scan lock: %w", err)
		}
		locks = append(locks, l)
	}
	return locks, rows.Err()
}
