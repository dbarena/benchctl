package runstate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps CGO_ENABLED=0 builds working
)

// migrations run in order to bring a database up to date. PRAGMA user_version
// holds the index of the last applied one, plus one.
//
// doc holds the entire State as one JSON document, so a new State field
// doesn't need a schema change. Every other column repeats a field of that
// same State so that List/ListActive can filter and sort in SQL.
var migrations = []string{
	// 1: initial schema.
	`CREATE TABLE runs (
	   run_id        TEXT PRIMARY KEY,
	   started_at    TEXT NOT NULL,
	   completed_at  TEXT,
	   terminated_at TEXT,
	   has_target    INTEGER NOT NULL DEFAULT 0,
	   created_by    TEXT NOT NULL DEFAULT '',
	   doc           TEXT NOT NULL
	 );
	 CREATE INDEX runs_started_at ON runs(started_at DESC);`,

	// 2: record ownership and read-through cache metadata. authoritative is 1
	// while this machine executes the run and 0 once it has been handed off to
	// a driver instance, which is what makes Load read through to that instance
	// instead of trusting the local copy.
	`ALTER TABLE runs ADD COLUMN authoritative INTEGER NOT NULL DEFAULT 1;
	 ALTER TABLE runs ADD COLUMN refreshed_at TEXT;
	 ALTER TABLE runs ADD COLUMN refresh_error TEXT;`,
}

// schemaVersion is the version this build writes and understands. benchctl
// rejects a database written by a newer build rather than misreading it.
var schemaVersion = len(migrations)

// LocalStore persists run state in a SQLite database at ~/.benchctl/state.db.
//
// A run handed off to a remote driver instance keeps executing there, so the
// local record would go stale. When dial is set, reads of such a record refresh
// from that instance over SSH and stay cached for refreshInterval; see
// relay.go.
type LocalStore struct {
	db   *sql.DB
	path string

	// NewStore sets dial and refreshInterval. A zero dial disables read-through
	// entirely, which tests and the driver-side process rely on.
	dial            Dialer
	refreshInterval time.Duration
	// refreshTimeout bounds one refresh; zero means defaultRefreshTimeout.
	refreshTimeout time.Duration
	// run executes SSH commands. Defaults to exec; injected by tests.
	run commandRunner
}

// NewLocalStore returns a LocalStore backed by ~/.benchctl/state.db.
func NewLocalStore() (*LocalStore, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("runstate: cannot determine home dir: %w", err)
	}
	return NewLocalStoreAt(filepath.Join(home, ".benchctl", "state.db"))
}

// NewLocalStoreAt returns a LocalStore backed by the database at path,
// creating it and its parent directory if needed.
//
// WAL plus a busy timeout is what makes concurrent access safe: a `benchctl
// status` or `wait` process can read while the run's own process writes, and
// _txlock=immediate makes Update take its write lock at BEGIN rather than
// mid-transaction, so a read-modify-write never has to be restarted.
func NewLocalStoreAt(path string) (*LocalStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("runstate: mkdir %s: %w", filepath.Dir(path), err)
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("runstate: open %s: %w", path, err)
	}
	s := &LocalStore{db: db, path: path, run: execCommand}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *LocalStore) Close() error { return s.db.Close() }

func (s *LocalStore) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("runstate: read schema version of %s: %w", s.path, err)
	}
	if version > schemaVersion {
		return fmt.Errorf("runstate: %s was written by a newer benchctl (schema version %d, this build understands %d); upgrade benchctl", s.path, version, schemaVersion)
	}
	for i := version; i < len(migrations); i++ {
		if _, err := s.db.Exec(migrations[i]); err != nil {
			return fmt.Errorf("runstate: apply migration %d to %s: %w", i+1, s.path, err)
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return fmt.Errorf("runstate: set schema version of %s: %w", s.path, err)
		}
	}
	return nil
}

const upsertColumns = `run_id, started_at, completed_at, terminated_at, has_target, created_by, doc`

// selectColumns is what a read needs: the record itself plus the bookkeeping
// columns that are not part of it.
const selectColumns = `doc, authoritative, refreshed_at, refresh_error`

// record is one row: a run state plus its ownership and cache bookkeeping.
type record struct {
	state         *State
	authoritative bool
	refreshedAt   time.Time
	refreshError  string
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanRecord(sc scanner) (record, error) {
	var (
		doc          string
		owned        bool
		refreshedAt  sql.NullString
		refreshError sql.NullString
	)
	if err := sc.Scan(&doc, &owned, &refreshedAt, &refreshError); err != nil {
		return record{}, err
	}
	state, err := parseDoc(doc)
	if err != nil {
		return record{}, err
	}
	rec := record{state: state, authoritative: owned, refreshError: refreshError.String}
	if refreshedAt.Valid {
		rec.refreshedAt, _ = time.Parse(time.RFC3339Nano, refreshedAt.String)
	}
	return rec, nil
}

// rowValues derives the indexed columns from state and marshals the record
// itself, in the column order of upsertColumns.
func rowValues(state *State) ([]any, error) {
	doc, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("runstate: marshal %s: %w", state.RunID, err)
	}
	hasTarget := 0
	// len, not != nil: an empty map does not survive the doc round trip
	// (omitempty), so deriving from length keeps the column and a reloaded
	// record in agreement.
	if len(state.TargetOutputs) > 0 {
		hasTarget = 1
	}
	return []any{
		state.RunID,
		state.StartedAt.UTC().Format(time.RFC3339Nano),
		nullableTime(state.CompletedAt),
		nullableTime(state.TerminatedAt),
		hasTarget,
		state.CreatedBy,
		string(doc),
	}, nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// Create inserts a new record. It fails rather than overwriting when the run
// ID is already present, since callers can supply their own ID via `benchctl
// run --async --run-id`. The check and the insert share one write transaction,
// so two processes cannot both win.
func (s *LocalStore) Create(state *State) error {
	values, err := rowValues(state)
	if err != nil {
		return err
	}
	return s.withTx(func(tx *sql.Tx) error {
		var exists int
		err := tx.QueryRow(`SELECT 1 FROM runs WHERE run_id = ?`, state.RunID).Scan(&exists)
		if err == nil {
			return fmt.Errorf("runstate: run %q already exists", state.RunID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("runstate: create %s: %w", state.RunID, err)
		}
		_, err = tx.Exec(`INSERT INTO runs (`+upsertColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)`, values...)
		if err != nil {
			return fmt.Errorf("runstate: create %s: %w", state.RunID, err)
		}
		return nil
	})
}

// Update loads the record for runID, applies fn, and writes the result back
// inside a single write transaction.
func (s *LocalStore) Update(runID string, fn func(*State)) error {
	return s.withTx(func(tx *sql.Tx) error {
		state, err := loadTx(tx, runID)
		if err != nil {
			return err
		}
		fn(state)
		values, err := rowValues(state)
		if err != nil {
			return err
		}
		// Shift run_id to the end for the WHERE clause.
		args := append(values[1:], values[0])
		_, err = tx.Exec(`UPDATE runs SET started_at = ?, completed_at = ?, terminated_at = ?, has_target = ?, created_by = ?, doc = ? WHERE run_id = ?`, args...)
		if err != nil {
			return fmt.Errorf("runstate: update %s: %w", runID, err)
		}
		return nil
	})
}

// Load returns the state for the given run ID, refreshed from the driver instance
// first if the run was handed off to one and the cached copy has expired.
func (s *LocalStore) Load(runID string) (*State, error) {
	rec, err := s.record(runID)
	if err != nil {
		return nil, err
	}
	return s.refresh(context.Background(), rec), nil
}

// List returns all run states, most recent first.
func (s *LocalStore) List() ([]*State, error) {
	recs, err := s.records(`SELECT ` + selectColumns + ` FROM runs ORDER BY started_at DESC`)
	if err != nil {
		return nil, err
	}
	return s.refreshAll(recs), nil
}

// The filter is the SQL twin of showsByDefault in cmd/benchctl/status.go: a
// run is active while its environment is still up.
func (s *LocalStore) ListActive() ([]*State, error) {
	recs, err := s.records(`SELECT ` + selectColumns + ` FROM runs
	                        WHERE completed_at IS NULL OR (terminated_at IS NULL AND has_target = 1)
	                        ORDER BY started_at DESC`)
	if err != nil {
		return nil, err
	}
	return s.refreshAll(recs), nil
}

// Delegate implements Owner. The run is now executed by another machine, so
// later reads must read through to it rather than trust the local copy.
func (s *LocalStore) Delegate(runID string) error {
	return s.setAuthoritative(runID, false)
}

// Claim implements Owner. This machine executes the run, so the local record
// is the authoritative one.
func (s *LocalStore) Claim(runID string) error {
	return s.setAuthoritative(runID, true)
}

func (s *LocalStore) setAuthoritative(runID string, owned bool) error {
	if _, err := s.db.Exec(`UPDATE runs SET authoritative = ? WHERE run_id = ?`, owned, runID); err != nil {
		return fmt.Errorf("runstate: set ownership of %s: %w", runID, err)
	}
	return nil
}

// Delete implements Purger. It removes the run record and nothing else.
// `benchctl teardown` tears down the infrastructure; purging metadata does not.
func (s *LocalStore) Delete(runID string) error {
	if _, err := s.db.Exec(`DELETE FROM runs WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("runstate: delete %s: %w", runID, err)
	}
	return nil
}

func (s *LocalStore) record(runID string) (record, error) {
	row := s.db.QueryRow(`SELECT `+selectColumns+` FROM runs WHERE run_id = ?`, runID)
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return record{}, fmt.Errorf("runstate: no state found for run %q: %w", runID, ErrRunNotFound)
	}
	if err != nil {
		return record{}, fmt.Errorf("runstate: load %s: %w", runID, err)
	}
	return rec, nil
}

func (s *LocalStore) records(q string, args ...any) ([]record, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("runstate: list: %w", err)
	}
	defer rows.Close()

	var recs []record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			continue // skip unreadable records, as the JSON store did
		}
		recs = append(recs, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("runstate: list: %w", err)
	}
	return recs, nil
}

func (s *LocalStore) withTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("runstate: begin transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("runstate: commit: %w", err)
	}
	return nil
}

func loadTx(tx *sql.Tx, runID string) (*State, error) {
	var doc string
	err := tx.QueryRow(`SELECT doc FROM runs WHERE run_id = ?`, runID).Scan(&doc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("runstate: no state found for run %q: %w", runID, ErrRunNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("runstate: load %s: %w", runID, err)
	}
	return parseDoc(doc)
}

func parseDoc(doc string) (*State, error) {
	var state State
	if err := json.Unmarshal([]byte(doc), &state); err != nil {
		return nil, fmt.Errorf("runstate: parse record: %w", err)
	}
	return &state, nil
}
