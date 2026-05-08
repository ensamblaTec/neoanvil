// Package coldstore implements an OLAP cold storage engine backed by SQLite. [SRE-38]
//
// Historical metrics, telemetry, and memex entries are archived from BoltDB
// (hot store) into SQLite (cold store) during REM sleep cycles. The cold store
// supports long-term analytics: trend queries, aggregation, and time-range scans
// without touching the operational BoltDB.
//
// Architecture:
//   Hot Store (BoltDB) → REM Archiver → Cold Store (SQLite)
//   Cold Store ← Analytics API (trend queries, aggregation)
package coldstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ensamblatec/neoanvil/pkg/config"
)

// Engine is the OLAP cold storage backed by SQLite. [SRE-38.1]
type Engine struct {
	db  *sql.DB
	cfg config.ColdstoreConfig
}

// MetricRecord represents a single archived metric entry. [SRE-38.2]
type MetricRecord struct {
	Timestamp   int64   `json:"timestamp"`
	Category    string  `json:"category"`    // "tool_latency", "gc_pressure", "arena_miss", etc.
	MetricName  string  `json:"metric_name"` // specific metric identifier
	Value       float64 `json:"value"`
	WorkspaceID string  `json:"workspace_id"`
	Metadata    string  `json:"metadata"` // JSON blob for extra context
}

// MemexArchive represents an archived memex entry. [SRE-38.2]
type MemexArchive struct {
	Timestamp   int64  `json:"timestamp"`
	Topic       string `json:"topic"`
	Scope       string `json:"scope"`
	Content     string `json:"content"`
	WorkspaceID string `json:"workspace_id"`
}

// TrendPoint is a single data point in a time-series trend query. [SRE-38.3]
type TrendPoint struct {
	Bucket    string  `json:"bucket"`    // time bucket label (e.g., "2026-04-10")
	AvgValue  float64 `json:"avg_value"`
	MaxValue  float64 `json:"max_value"`
	MinValue  float64 `json:"min_value"`
	Count     int64   `json:"count"`
}

// OpenEngine opens or creates the cold storage SQLite database. [SRE-38.1]
func OpenEngine(dbPath string, ccfg config.ColdstoreConfig) (*Engine, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("coldstore: open %s: %w", dbPath, err)
	}

	db.SetMaxOpenConns(ccfg.MaxOpenConns)
	db.SetMaxIdleConns(ccfg.MaxIdleConns)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("coldstore: ping: %w", err)
	}

	e := &Engine{db: db, cfg: ccfg}
	if err := e.migrate(); err != nil {
		return nil, fmt.Errorf("coldstore: migrate: %w", err)
	}
	return e, nil
}

// migrate creates the schema if it doesn't exist. [SRE-38.1]
func (e *Engine) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS metrics (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp INTEGER NOT NULL,
			category TEXT NOT NULL,
			metric_name TEXT NOT NULL,
			value REAL NOT NULL,
			workspace_id TEXT DEFAULT '',
			metadata TEXT DEFAULT '',
			created_at TEXT DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_metrics_ts ON metrics(timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_metrics_cat ON metrics(category, metric_name)`,
		`CREATE INDEX IF NOT EXISTS idx_metrics_ws ON metrics(workspace_id, timestamp)`,

		`CREATE TABLE IF NOT EXISTS memex_archive (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp INTEGER NOT NULL,
			topic TEXT NOT NULL,
			scope TEXT NOT NULL,
			content TEXT NOT NULL,
			workspace_id TEXT DEFAULT '',
			created_at TEXT DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memex_ts ON memex_archive(timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_memex_topic ON memex_archive(topic)`,

		`CREATE TABLE IF NOT EXISTS tech_debt_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp INTEGER NOT NULL,
			file_path TEXT NOT NULL,
			complexity_score REAL NOT NULL,
			mutation_count INTEGER NOT NULL,
			workspace_id TEXT DEFAULT '',
			created_at TEXT DEFAULT (datetime('now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_debt_ts ON tech_debt_snapshots(timestamp)`,
		`CREATE INDEX IF NOT EXISTS idx_debt_file ON tech_debt_snapshots(file_path)`,
	}

	for _, stmt := range stmts {
		if _, err := e.db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", stmt[:40], err)
		}
	}
	return nil
}

// ─── Archive (Ingestion from Hot Store) [SRE-38.2] ─────────────────────────

// ArchiveMetrics bulk-inserts metric records into cold storage.
func (e *Engine) ArchiveMetrics(ctx context.Context, records []MetricRecord) (int, error) {
	if len(records) == 0 {
		return 0, nil
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO metrics (timestamp, category, metric_name, value, workspace_id, metadata)
		 VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	count := 0
	for _, r := range records {
		if _, err := stmt.ExecContext(ctx, r.Timestamp, r.Category, r.MetricName, r.Value, r.WorkspaceID, r.Metadata); err != nil {
			return 0, fmt.Errorf("coldstore archive metric %s/%s: %w", r.Category, r.MetricName, err)
		}
		count++
	}

	return count, tx.Commit()
}

// ArchiveMemex bulk-inserts memex entries into cold storage.
func (e *Engine) ArchiveMemex(ctx context.Context, entries []MemexArchive) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO memex_archive (timestamp, topic, scope, content, workspace_id)
		 VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	count := 0
	for _, m := range entries {
		if _, err := stmt.ExecContext(ctx, m.Timestamp, m.Topic, m.Scope, m.Content, m.WorkspaceID); err != nil {
			return 0, fmt.Errorf("coldstore archive memex %s: %w", m.Topic, err)
		}
		count++
	}

	return count, tx.Commit()
}

// ArchiveTechDebt snapshots current tech debt scores for trend tracking.
func (e *Engine) ArchiveTechDebt(ctx context.Context, workspace string, fileScores map[string]float64, mutationCounts map[string]int) (int, error) {
	if len(fileScores) == 0 {
		return 0, nil
	}

	tx, err := e.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO tech_debt_snapshots (timestamp, file_path, complexity_score, mutation_count, workspace_id)
		 VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	now := time.Now().Unix()
	count := 0
	for path, score := range fileScores {
		mutations := mutationCounts[path]
		if _, err := stmt.ExecContext(ctx, now, path, score, mutations, workspace); err != nil {
			continue
		}
		count++
	}

	return count, tx.Commit()
}

// ─── Analytics API [SRE-38.3] ──────────────────────────────────────────────

// QueryMetricTrend returns aggregated metric values over time buckets. [SRE-38.3]
// granularity: "hour", "day", "week", "month"
func (e *Engine) QueryMetricTrend(ctx context.Context, category, metricName, granularity string, since, until time.Time) ([]TrendPoint, error) {
	var strftimeFmt string
	switch granularity {
	case "hour":
		strftimeFmt = "%Y-%m-%d %H:00"
	case "week":
		strftimeFmt = "%Y-W%W"
	case "month":
		strftimeFmt = "%Y-%m"
	default: // "day"
		strftimeFmt = "%Y-%m-%d"
	}

	query := fmt.Sprintf(`
		SELECT strftime('%s', datetime(timestamp, 'unixepoch')) as bucket,
			   AVG(value) as avg_val,
			   MAX(value) as max_val,
			   MIN(value) as min_val,
			   COUNT(*) as cnt
		FROM metrics
		WHERE category = ? AND metric_name = ?
		  AND timestamp >= ? AND timestamp <= ?
		GROUP BY bucket
		ORDER BY bucket`, strftimeFmt)

	rows, err := e.db.QueryContext(ctx, query, category, metricName, since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []TrendPoint
	for rows.Next() {
		var tp TrendPoint
		if err := rows.Scan(&tp.Bucket, &tp.AvgValue, &tp.MaxValue, &tp.MinValue, &tp.Count); err != nil {
			continue
		}
		results = append(results, tp)
	}
	return results, rows.Err()
}

// QueryTechDebtTrend returns tech debt evolution for a file over time. [SRE-38.3]
func (e *Engine) QueryTechDebtTrend(ctx context.Context, filePath string, since, until time.Time) ([]TrendPoint, error) {
	query := `
		SELECT strftime('%Y-%m-%d', datetime(timestamp, 'unixepoch')) as bucket,
			   AVG(complexity_score) as avg_val,
			   MAX(complexity_score) as max_val,
			   MIN(complexity_score) as min_val,
			   COUNT(*) as cnt
		FROM tech_debt_snapshots
		WHERE file_path = ? AND timestamp >= ? AND timestamp <= ?
		GROUP BY bucket
		ORDER BY bucket`

	rows, err := e.db.QueryContext(ctx, query, filePath, since.Unix(), until.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []TrendPoint
	for rows.Next() {
		var tp TrendPoint
		if err := rows.Scan(&tp.Bucket, &tp.AvgValue, &tp.MaxValue, &tp.MinValue, &tp.Count); err != nil {
			continue
		}
		results = append(results, tp)
	}
	return results, rows.Err()
}

// QueryMemexByTopic searches archived memex entries by topic pattern. [SRE-38.3]
func (e *Engine) QueryMemexByTopic(ctx context.Context, topicPattern string, limit int) ([]MemexArchive, error) {
	if limit <= 0 {
		limit = e.cfg.DefaultQueryLimit
		if limit <= 0 {
			limit = 50
		}
	}
	query := `SELECT timestamp, topic, scope, content, workspace_id
			  FROM memex_archive
			  WHERE topic LIKE ?
			  ORDER BY timestamp DESC
			  LIMIT ?`

	rows, err := e.db.QueryContext(ctx, query, "%"+topicPattern+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []MemexArchive
	for rows.Next() {
		var m MemexArchive
		if err := rows.Scan(&m.Timestamp, &m.Topic, &m.Scope, &m.Content, &m.WorkspaceID); err != nil {
			continue
		}
		results = append(results, m)
	}
	return results, rows.Err()
}

// Summary returns a high-level summary of the cold store contents. [SRE-38.3]
type StoreSummary struct {
	MetricCount    int64  `json:"metric_count"`
	MemexCount     int64  `json:"memex_count"`
	DebtSnapshots  int64  `json:"debt_snapshots"`
	OldestMetric   string `json:"oldest_metric"`
	NewestMetric   string `json:"newest_metric"`
	DBSizeBytes    int64  `json:"db_size_bytes"`
}

func (e *Engine) Summary(ctx context.Context) (*StoreSummary, error) {
	s := &StoreSummary{}

	if err := e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM metrics").Scan(&s.MetricCount); err != nil {
		return nil, fmt.Errorf("coldstore summary metrics: %w", err)
	}
	if err := e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memex_archive").Scan(&s.MemexCount); err != nil {
		return nil, fmt.Errorf("coldstore summary memex: %w", err)
	}
	if err := e.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM tech_debt_snapshots").Scan(&s.DebtSnapshots); err != nil {
		return nil, fmt.Errorf("coldstore summary debt: %w", err)
	}
	// These may return NULL on empty tables — use COALESCE
	e.db.QueryRowContext(ctx, "SELECT COALESCE(datetime(MIN(timestamp), 'unixepoch'), '') FROM metrics").Scan(&s.OldestMetric)
	e.db.QueryRowContext(ctx, "SELECT COALESCE(datetime(MAX(timestamp), 'unixepoch'), '') FROM metrics").Scan(&s.NewestMetric)

	var pageCount, pageSize int64
	e.db.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pageCount)
	e.db.QueryRowContext(ctx, "PRAGMA page_size").Scan(&pageSize)
	s.DBSizeBytes = pageCount * pageSize

	return s, nil
}

// PurgeOlderThan deletes records older than the given timestamp. [SRE-38.2]
func (e *Engine) PurgeOlderThan(ctx context.Context, before time.Time) (int64, error) {
	ts := before.Unix()
	var total int64
	var firstErr error

	tables := []string{"metrics", "memex_archive", "tech_debt_snapshots"}
	for _, table := range tables {
		res, err := e.db.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE timestamp < ?", table), ts)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("purge %s: %w", table, err)
			}
			continue
		}
		n, _ := res.RowsAffected()
		total += n
	}

	// Reclaim space
	_, _ = e.db.ExecContext(ctx, "VACUUM")

	return total, firstErr
}

// Close shuts down the cold storage engine.
func (e *Engine) Close() error {
	return e.db.Close()
}
