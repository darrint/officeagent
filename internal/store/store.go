package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Store is a thin SQLite-backed key-value store used for persisting tokens
// and other small pieces of application state.
type Store struct {
	db *sql.DB
}

// Feedback is a single piece of user feedback on a generated summary.
type Feedback struct {
	ID        int64
	Section   string // "email" or "calendar"
	Rating    string // "good" or "bad"
	Note      string
	CreatedAt time.Time
}

// Report is a historical briefing record.
type Report struct {
	ID          int64
	GeneratedAt time.Time
	Content     string // JSON blob of the full cachedReport
}

// New opens (or creates) the SQLite database at path and runs migrations.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// SQLite does not support concurrent writers. Limiting the pool to a single
	// connection also ensures that :memory: databases (used in tests) are not
	// split across multiple connections, each of which would see an empty DB.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	// WAL mode allows concurrent readers alongside a single writer, dramatically
	// reducing SQLITE_BUSY errors when multiple goroutines access the store.
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return fmt.Errorf("set WAL mode: %w", err)
	}
	// Wait up to 5 seconds before returning SQLITE_BUSY instead of failing
	// immediately. This covers short write contention (e.g. the doctor check
	// writing a ping key while a handler reads prompts).
	if _, err := s.db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		return fmt.Errorf("set busy timeout: %w", err)
	}
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS kv (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create kv table: %w", err)
	}
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS feedback (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			section    TEXT NOT NULL,
			rating     TEXT NOT NULL,
			note       TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		return fmt.Errorf("create feedback table: %w", err)
	}
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS reports (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			generated_at TEXT NOT NULL,
			content      TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create reports table: %w", err)
	}
	return nil
}

// Get returns the value for key, or "" if not found.
func (s *Store) Get(key string) (string, error) {
	var value string
	err := s.db.QueryRow("SELECT value FROM kv WHERE key = ?", key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// Set upserts key=value.
func (s *Store) Set(key, value string) error {
	_, err := s.db.Exec(
		"INSERT INTO kv(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		key, value,
	)
	return err
}

// AddFeedback records a user feedback entry for a summary section.
func (s *Store) AddFeedback(section, rating, note string) error {
	_, err := s.db.Exec(
		"INSERT INTO feedback(section, rating, note) VALUES(?, ?, ?)",
		section, rating, note,
	)
	return err
}

// RecentFeedback returns the most recent limit feedback entries for section,
// newest first.
func (s *Store) RecentFeedback(section string, limit int) ([]Feedback, error) {
	rows, err := s.db.Query(
		"SELECT id, section, rating, note, created_at FROM feedback WHERE section = ? ORDER BY id DESC LIMIT ?",
		section, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []Feedback
	for rows.Next() {
		var f Feedback
		var createdAt string
		if err := rows.Scan(&f.ID, &f.Section, &f.Rating, &f.Note, &createdAt); err != nil {
			return nil, err
		}
		f.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		results = append(results, f)
	}
	return results, rows.Err()
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// SaveReport inserts a historical report record and returns the assigned ID.
func (s *Store) SaveReport(generatedAt time.Time, content string) (int64, error) {
	res, err := s.db.Exec(
		"INSERT INTO reports(generated_at, content) VALUES(?, ?)",
		generatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		content,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListReports returns all historical report records, newest first.
// Only id and generated_at are populated (content is omitted for efficiency).
func (s *Store) ListReports() ([]Report, error) {
	rows, err := s.db.Query(
		"SELECT id, generated_at FROM reports ORDER BY id DESC",
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var results []Report
	for rows.Next() {
		var r Report
		var ts string
		if err := rows.Scan(&r.ID, &ts); err != nil {
			return nil, err
		}
		r.GeneratedAt, _ = time.Parse("2006-01-02T15:04:05Z", ts)
		results = append(results, r)
	}
	return results, rows.Err()
}

// GetReport returns a single report by ID, including content.
func (s *Store) GetReport(id int64) (*Report, error) {
	var r Report
	var ts string
	err := s.db.QueryRow(
		"SELECT id, generated_at, content FROM reports WHERE id = ?", id,
	).Scan(&r.ID, &ts, &r.Content)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.GeneratedAt, _ = time.Parse("2006-01-02T15:04:05Z", ts)
	return &r, nil
}
