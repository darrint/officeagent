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

// New opens (or creates) the SQLite database at path and runs migrations.
func New(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate db: %w", err)
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS kv (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS feedback (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			section    TEXT NOT NULL,
			rating     TEXT NOT NULL,
			note       TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
	`)
	return err
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
