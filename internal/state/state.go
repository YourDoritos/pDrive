// Package state is pDrive's sync state database.
//
// The `nodes` table is the *baseline*: the last state on which the local
// filesystem and Proton Drive agreed. Reconciliation compares local against
// baseline and remote against baseline, so the baseline is what makes
// "changed on both sides" distinguishable from "changed on one".
//
// Everything here is deliberately boring and synchronous. A sync engine that
// loses track of what it already agreed to is a sync engine that deletes
// files.
package state

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	// Pure-Go SQLite driver: no cgo, so pDrive stays a static binary.
	_ "modernc.org/sqlite"
)

// SchemaVersion is bumped whenever migrations are added.
const SchemaVersion = 1

// Materialization records whether a node's bytes are actually on disk.
type Materialization int

const (
	// NotMaterialized means only a stub stands in for the file: it exceeded
	// max_auto_download_size and has not been fetched.
	NotMaterialized Materialization = 0
	// Materialized means the real content is on disk.
	Materialized Materialization = 1
)

// Node is one row of the baseline.
type Node struct {
	Path         string
	NodeID       string
	ParentID     string
	IsDir        bool
	RevisionID   string
	ContentHash  string
	Size         int64
	LocalMtime   time.Time
	LocalInode   uint64
	Materialized Materialization
	SyncedAt     time.Time
}

// DB is the sync state database.
type DB struct {
	db *sql.DB
}

// Open opens (creating if needed) the state database and applies migrations.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}

	// _txlock=immediate avoids SQLITE_BUSY upgrade deadlocks between the
	// daemon's reconciler and a concurrent CLI reader.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	// One writer. SQLite allows more, but serialising writes here removes a
	// whole class of interleaving bug from the reconciler.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open state db: %w", err)
	}

	s := &DB{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil && !os.IsNotExist(err) {
		s.Close()
		return nil, fmt.Errorf("secure state db: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *DB) Close() error { return s.db.Close() }

const schemaV1 = `
CREATE TABLE IF NOT EXISTS nodes (
  path            TEXT PRIMARY KEY,
  node_id         TEXT NOT NULL,
  parent_id       TEXT NOT NULL DEFAULT '',
  is_dir          INTEGER NOT NULL DEFAULT 0,
  revision_id     TEXT NOT NULL DEFAULT '',
  content_hash    TEXT NOT NULL DEFAULT '',
  size            INTEGER NOT NULL DEFAULT 0,
  local_mtime_ns  INTEGER NOT NULL DEFAULT 0,
  local_inode     INTEGER NOT NULL DEFAULT 0,
  materialized    INTEGER NOT NULL DEFAULT 1,
  synced_at       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS nodes_by_node_id ON nodes(node_id);
CREATE INDEX IF NOT EXISTS nodes_by_inode   ON nodes(local_inode);
CREATE INDEX IF NOT EXISTS nodes_by_parent  ON nodes(parent_id);

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS pending (
  path         TEXT PRIMARY KEY,
  direction    TEXT NOT NULL,
  revision_id  TEXT NOT NULL DEFAULT '',
  blocks_done  INTEGER NOT NULL DEFAULT 0,
  total_blocks INTEGER NOT NULL DEFAULT 0,
  updated_at   INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS conflicts (
  path            TEXT NOT NULL,
  kept_local      TEXT NOT NULL,
  remote_revision TEXT NOT NULL DEFAULT '',
  at              INTEGER NOT NULL DEFAULT 0
);
`

func (s *DB) migrate() error {
	if _, err := s.db.Exec(schemaV1); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}

	current, err := s.GetMetaInt(KeySchemaVersion)
	if err != nil {
		return err
	}
	if current > SchemaVersion {
		return fmt.Errorf("state database is schema v%d but this pDrive only understands v%d — upgrade pDrive",
			current, SchemaVersion)
	}
	if current != SchemaVersion {
		if err := s.SetMetaInt(KeySchemaVersion, SchemaVersion); err != nil {
			return err
		}
	}
	return nil
}

// Meta keys.
const (
	KeySchemaVersion = "schema_version"
	KeyEventCursor   = "event_cursor"
	KeyVolumeID      = "volume_id"
	KeyShareID       = "share_id"
	KeyRootLinkID    = "root_link_id"
	KeyAccount       = "account"
	KeyLastFullScan  = "last_full_scan"
)

// GetMeta reads a metadata value. A missing key returns "" with no error.
func (s *DB) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read meta %q: %w", key, err)
	}
	return v, nil
}

// SetMeta writes a metadata value.
func (s *DB) SetMeta(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO meta(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("write meta %q: %w", key, err)
	}
	return nil
}

// GetMetaInt reads a metadata value as an integer. Missing or unparsable
// returns 0.
func (s *DB) GetMetaInt(key string) (int, error) {
	v, err := s.GetMeta(key)
	if err != nil || v == "" {
		return 0, err
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return 0, nil
	}
	return n, nil
}

// SetMetaInt writes an integer metadata value.
func (s *DB) SetMetaInt(key string, n int) error {
	return s.SetMeta(key, fmt.Sprintf("%d", n))
}

// PutNode inserts or replaces a baseline row.
func (s *DB) PutNode(n Node) error {
	var mtime int64
	if !n.LocalMtime.IsZero() {
		mtime = n.LocalMtime.UnixNano()
	}
	var synced int64
	if n.SyncedAt.IsZero() {
		synced = time.Now().Unix()
	} else {
		synced = n.SyncedAt.Unix()
	}

	_, err := s.db.Exec(`
		INSERT INTO nodes(path, node_id, parent_id, is_dir, revision_id, content_hash,
		                  size, local_mtime_ns, local_inode, materialized, synced_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			node_id = excluded.node_id, parent_id = excluded.parent_id,
			is_dir = excluded.is_dir, revision_id = excluded.revision_id,
			content_hash = excluded.content_hash, size = excluded.size,
			local_mtime_ns = excluded.local_mtime_ns, local_inode = excluded.local_inode,
			materialized = excluded.materialized, synced_at = excluded.synced_at`,
		n.Path, n.NodeID, n.ParentID, boolToInt(n.IsDir), n.RevisionID, n.ContentHash,
		n.Size, mtime, int64(n.LocalInode), int(n.Materialized), synced)
	if err != nil {
		return fmt.Errorf("put node %q: %w", n.Path, err)
	}
	return nil
}

// GetNode reads one baseline row. A missing path returns (nil, nil).
func (s *DB) GetNode(path string) (*Node, error) {
	row := s.db.QueryRow(`
		SELECT path, node_id, parent_id, is_dir, revision_id, content_hash,
		       size, local_mtime_ns, local_inode, materialized, synced_at
		FROM nodes WHERE path = ?`, path)

	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get node %q: %w", path, err)
	}
	return n, nil
}

// GetNodeByID reads a baseline row by Proton link ID.
func (s *DB) GetNodeByID(nodeID string) (*Node, error) {
	row := s.db.QueryRow(`
		SELECT path, node_id, parent_id, is_dir, revision_id, content_hash,
		       size, local_mtime_ns, local_inode, materialized, synced_at
		FROM nodes WHERE node_id = ? LIMIT 1`, nodeID)

	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get node by id %q: %w", nodeID, err)
	}
	return n, nil
}

// DeleteNode removes a baseline row.
func (s *DB) DeleteNode(path string) error {
	if _, err := s.db.Exec(`DELETE FROM nodes WHERE path = ?`, path); err != nil {
		return fmt.Errorf("delete node %q: %w", path, err)
	}
	return nil
}

// DeleteSubtree removes a path and everything beneath it.
func (s *DB) DeleteSubtree(path string) error {
	if _, err := s.db.Exec(`DELETE FROM nodes WHERE path = ? OR path LIKE ? ESCAPE '\'`,
		path, escapeLike(path)+"/%"); err != nil {
		return fmt.Errorf("delete subtree %q: %w", path, err)
	}
	return nil
}

// AllNodes returns every baseline row, ordered by path.
func (s *DB) AllNodes() ([]Node, error) {
	rows, err := s.db.Query(`
		SELECT path, node_id, parent_id, is_dir, revision_id, content_hash,
		       size, local_mtime_ns, local_inode, materialized, synced_at
		FROM nodes ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	defer rows.Close()

	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		out = append(out, *n)
	}
	return out, rows.Err()
}

// CountNodes returns the number of baseline rows.
func (s *DB) CountNodes() (int, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count nodes: %w", err)
	}
	return n, nil
}

// Stats summarises the baseline.
type Stats struct {
	Files         int
	Dirs          int
	Stubs         int
	Bytes         int64
	MaterialBytes int64
}

// Stats computes a summary of the baseline.
func (s *DB) Stats() (*Stats, error) {
	var st Stats
	err := s.db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN is_dir = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN is_dir = 1 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN is_dir = 0 AND materialized = 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN is_dir = 0 THEN size ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN is_dir = 0 AND materialized = 1 THEN size ELSE 0 END), 0)
		FROM nodes`).Scan(&st.Files, &st.Dirs, &st.Stubs, &st.Bytes, &st.MaterialBytes)
	if err != nil {
		return nil, fmt.Errorf("stats: %w", err)
	}
	return &st, nil
}

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanNode(r rowScanner) (*Node, error) {
	var (
		n        Node
		isDir    int
		mtimeNS  int64
		inode    int64
		material int
		synced   int64
	)
	err := r.Scan(&n.Path, &n.NodeID, &n.ParentID, &isDir, &n.RevisionID, &n.ContentHash,
		&n.Size, &mtimeNS, &inode, &material, &synced)
	if err != nil {
		return nil, err
	}
	n.IsDir = isDir != 0
	if mtimeNS != 0 {
		n.LocalMtime = time.Unix(0, mtimeNS)
	}
	n.LocalInode = uint64(inode)
	n.Materialized = Materialization(material)
	if synced != 0 {
		n.SyncedAt = time.Unix(synced, 0)
	}
	return &n, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// escapeLike escapes LIKE wildcards so a path containing % or _ cannot match
// unrelated rows during a subtree delete.
func escapeLike(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '%' || r == '_' || r == '\\' {
			out = append(out, '\\')
		}
		out = append(out, r)
	}
	return string(out)
}

// RecordConflict logs a preserved local copy.
func (s *DB) RecordConflict(path, keptLocal, remoteRevision string) error {
	_, err := s.db.Exec(
		`INSERT INTO conflicts(path, kept_local, remote_revision, at) VALUES(?, ?, ?, ?)`,
		path, keptLocal, remoteRevision, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("record conflict for %q: %w", path, err)
	}
	return nil
}

// Conflict is one logged conflict.
type Conflict struct {
	Path      string
	KeptLocal string
	RemoteRev string
	At        time.Time
}

// Conflicts returns logged conflicts, newest first.
func (s *DB) Conflicts() ([]Conflict, error) {
	rows, err := s.db.Query(
		`SELECT path, kept_local, remote_revision, at FROM conflicts ORDER BY at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list conflicts: %w", err)
	}
	defer rows.Close()

	var out []Conflict
	for rows.Next() {
		var c Conflict
		var at int64
		if err := rows.Scan(&c.Path, &c.KeptLocal, &c.RemoteRev, &at); err != nil {
			return nil, err
		}
		if at != 0 {
			c.At = time.Unix(at, 0)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConflict removes a conflict record.
func (s *DB) DeleteConflict(path, keptLocal string) error {
	_, err := s.db.Exec(`DELETE FROM conflicts WHERE path = ? AND kept_local = ?`,
		path, keptLocal)
	if err != nil {
		return fmt.Errorf("delete conflict for %q: %w", path, err)
	}
	return nil
}
