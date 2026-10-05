package manifest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Record is one asset's manifest row.
type Record struct {
	AssetID      string
	FileName     string
	State        State
	ResumeFrom   State // progress state to restart from after Failed/NeedsConfirm
	OriginalSize int64
	NewSize      int64
	CRF          int
	BackupPath   string
	OutputPath   string
	NewAssetID   string
	Error        string
	People       string // named people on the original, comma separated
	Detail       string
	UpdatedAt    time.Time
}

// Store is the SQLite manifest.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS assets (
	asset_id      TEXT PRIMARY KEY,
	file_name     TEXT NOT NULL DEFAULT '',
	state         TEXT NOT NULL,
	resume_from   TEXT NOT NULL DEFAULT '',
	original_size INTEGER NOT NULL DEFAULT 0,
	new_size      INTEGER NOT NULL DEFAULT 0,
	crf           INTEGER NOT NULL DEFAULT 0,
	backup_path   TEXT NOT NULL DEFAULT '',
	output_path   TEXT NOT NULL DEFAULT '',
	new_asset_id  TEXT NOT NULL DEFAULT '',
	error         TEXT NOT NULL DEFAULT '',
	people        TEXT NOT NULL DEFAULT '',
	detail        TEXT NOT NULL DEFAULT '',
	created_at    TEXT NOT NULL,
	updated_at    TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	asset_id   TEXT NOT NULL,
	at         TEXT NOT NULL,
	from_state TEXT NOT NULL,
	to_state   TEXT NOT NULL,
	message    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_asset ON events(asset_id);
`

// Open opens or creates the manifest at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection serialises writers from concurrent workers.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init manifest %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const cols = `asset_id, file_name, state, resume_from, original_size, new_size, crf,
	backup_path, output_path, new_asset_id, error, people, detail, updated_at`

func scan(row interface{ Scan(...any) error }) (Record, error) {
	var r Record
	var updated string
	err := row.Scan(&r.AssetID, &r.FileName, &r.State, &r.ResumeFrom, &r.OriginalSize, &r.NewSize,
		&r.CRF, &r.BackupPath, &r.OutputPath, &r.NewAssetID, &r.Error, &r.People, &r.Detail, &updated)
	if err != nil {
		return r, err
	}
	r.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return r, nil
}

// Get returns the record for id, or ok=false if there is none.
func (s *Store) Get(ctx context.Context, id string) (Record, bool, error) {
	r, err := scan(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM assets WHERE asset_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	return r, err == nil, err
}

// Ensure returns the existing record for id or inserts a Pending one.
func (s *Store) Ensure(ctx context.Context, id, fileName string, size int64) (Record, error) {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `INSERT INTO assets (asset_id, file_name, state, original_size, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(asset_id) DO NOTHING`, id, fileName, Pending, size, now, now)
	if err != nil {
		return Record{}, err
	}
	r, _, err := s.Get(ctx, id)
	return r, err
}

// Transition validates and applies a state change to r, writing all of r's
// fields and an event row in one transaction. r.State is updated on success.
func (s *Store) Transition(ctx context.Context, r *Record, to State, msg string) error {
	if err := CheckTransition(r.State, to); err != nil {
		return err
	}
	// The manifest must be written even if the run is being cancelled.
	ctx = context.WithoutCancel(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `UPDATE assets SET file_name=?, state=?, resume_from=?, original_size=?,
		new_size=?, crf=?, backup_path=?, output_path=?, new_asset_id=?, error=?, people=?, detail=?, updated_at=?
		WHERE asset_id=? AND state=?`,
		r.FileName, to, r.ResumeFrom, r.OriginalSize, r.NewSize, r.CRF, r.BackupPath, r.OutputPath,
		r.NewAssetID, r.Error, r.People, r.Detail, now.Format(time.RFC3339Nano), r.AssetID, r.State)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("manifest row for %s is not in state %s (changed by another process?)", r.AssetID, r.State)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (asset_id, at, from_state, to_state, message) VALUES (?, ?, ?, ?, ?)`,
		r.AssetID, now.Format(time.RFC3339Nano), r.State, to, msg); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	r.State = to
	r.UpdatedAt = now
	return nil
}

// List returns all records, most recently updated first.
func (s *Store) List(ctx context.Context) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+cols+` FROM assets ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		r, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// States returns asset id → state for every record.
func (s *Store) States(ctx context.Context) (map[string]State, error) {
	recs, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]State, len(recs))
	for _, r := range recs {
		m[r.AssetID] = r.State
	}
	return m, nil
}
