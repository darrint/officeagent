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

// FeedEvent is a single raw event collected by the background poller.
type FeedEvent struct {
	ID         int64
	Source     string    // "graph_mail" | "graph_calendar" | "github" | "fastmail"
	ExternalID string    // message/PR/event ID from the source API (for dedup)
	Payload    string    // JSON blob with source-specific fields
	OccurredAt time.Time // timestamp of the event from the source API
	CardID     int64     // 0 until included in a FeedCard
}

// FeedCard is an LLM-generated summary of a group of FeedEvents.
type FeedCard struct {
	ID          int64
	Source      string
	SummaryMD   string    // raw LLM markdown
	SummaryHTML string    // rendered HTML
	EventCount  int
	TimeLabel   string    // human label chosen by LLM, e.g. "This morning"
	OldestAt    time.Time
	NewestAt    time.Time
	CreatedAt   time.Time
}

// FeedState holds per-source delta/sync tokens for incremental polling.
type FeedState struct {
	Source     string
	DeltaToken string    // API-specific cursor (delta link, JMAP state, RFC3339 timestamp)
	LastPoll   time.Time // zero if never polled
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
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS feed_events (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			source      TEXT NOT NULL,
			external_id TEXT NOT NULL,
			payload     TEXT NOT NULL,
			occurred_at TEXT NOT NULL,
			card_id     INTEGER,
			UNIQUE(source, external_id)
		)
	`); err != nil {
		return fmt.Errorf("create feed_events table: %w", err)
	}
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS feed_cards (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			source       TEXT NOT NULL,
			summary_md   TEXT NOT NULL,
			summary_html TEXT NOT NULL,
			event_count  INTEGER NOT NULL,
			time_label   TEXT NOT NULL,
			oldest_at    TEXT NOT NULL,
			newest_at    TEXT NOT NULL,
			created_at   TEXT NOT NULL DEFAULT (datetime('now'))
		)
	`); err != nil {
		return fmt.Errorf("create feed_cards table: %w", err)
	}
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS feed_state (
			source      TEXT PRIMARY KEY,
			delta_token TEXT NOT NULL DEFAULT '',
			last_poll   TEXT NOT NULL DEFAULT ''
		)
	`); err != nil {
		return fmt.Errorf("create feed_state table: %w", err)
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

// SaveFeedEvent inserts a new feed event if it does not already exist
// (source+external_id must be unique). Returns true if a new row was inserted.
func (s *Store) SaveFeedEvent(source, externalID, payload string, occurredAt time.Time) (bool, error) {
	res, err := s.db.Exec(
		`INSERT INTO feed_events(source, external_id, payload, occurred_at)
		 VALUES(?, ?, ?, ?)
		 ON CONFLICT(source, external_id) DO NOTHING`,
		source, externalID, payload,
		occurredAt.UTC().Format("2006-01-02T15:04:05Z"),
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListUnseenFeedEvents returns all feed events for source that have not yet
// been associated with a FeedCard, ordered oldest first.
func (s *Store) ListUnseenFeedEvents(source string) ([]FeedEvent, error) {
	rows, err := s.db.Query(
		`SELECT id, source, external_id, payload, occurred_at, card_id
		 FROM feed_events
		 WHERE source = ? AND card_id IS NULL
		 ORDER BY occurred_at ASC`,
		source,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanFeedEvents(rows)
}

// CountUnseenFeedEvents returns the number of unseen events per source.
func (s *Store) CountUnseenFeedEvents() (map[string]int, error) {
	rows, err := s.db.Query(
		`SELECT source, COUNT(*) FROM feed_events WHERE card_id IS NULL GROUP BY source`,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[string]int)
	for rows.Next() {
		var src string
		var n int
		if err := rows.Scan(&src, &n); err != nil {
			return nil, err
		}
		counts[src] = n
	}
	return counts, rows.Err()
}

// SaveFeedCard inserts a new FeedCard and returns its assigned ID.
func (s *Store) SaveFeedCard(card FeedCard) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO feed_cards(source, summary_md, summary_html, event_count, time_label, oldest_at, newest_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?)`,
		card.Source,
		card.SummaryMD,
		card.SummaryHTML,
		card.EventCount,
		card.TimeLabel,
		card.OldestAt.UTC().Format("2006-01-02T15:04:05Z"),
		card.NewestAt.UTC().Format("2006-01-02T15:04:05Z"),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// MarkFeedEventsWithCard sets card_id on all feed_events whose IDs are in eventIDs.
func (s *Store) MarkFeedEventsWithCard(eventIDs []int64, cardID int64) error {
	if len(eventIDs) == 0 {
		return nil
	}
	// Build IN clause manually; SQLite driver does not expand slices.
	query := "UPDATE feed_events SET card_id = ? WHERE id IN ("
	args := make([]any, 0, len(eventIDs)+1)
	args = append(args, cardID)
	for i, id := range eventIDs {
		if i > 0 {
			query += ","
		}
		query += "?"
		args = append(args, id)
	}
	query += ")"
	_, err := s.db.Exec(query, args...)
	return err
}

// ListFeedCards returns up to limit feed cards, newest first.
// If beforeID > 0 only cards with id < beforeID are returned (cursor pagination).
func (s *Store) ListFeedCards(limit int, beforeID int64) ([]FeedCard, error) {
	var rows *sql.Rows
	var err error
	if beforeID > 0 {
		rows, err = s.db.Query(
			`SELECT id, source, summary_md, summary_html, event_count, time_label, oldest_at, newest_at, created_at
			 FROM feed_cards WHERE id < ? ORDER BY id DESC LIMIT ?`,
			beforeID, limit,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, source, summary_md, summary_html, event_count, time_label, oldest_at, newest_at, created_at
			 FROM feed_cards ORDER BY id DESC LIMIT ?`,
			limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanFeedCards(rows)
}

// GetFeedState returns the stored FeedState for source.
// Returns a zero FeedState (empty DeltaToken, zero LastPoll) if no row exists yet.
func (s *Store) GetFeedState(source string) (FeedState, error) {
	var fs FeedState
	var lastPoll string
	err := s.db.QueryRow(
		`SELECT source, delta_token, last_poll FROM feed_state WHERE source = ?`, source,
	).Scan(&fs.Source, &fs.DeltaToken, &lastPoll)
	if err == sql.ErrNoRows {
		return FeedState{Source: source}, nil
	}
	if err != nil {
		return FeedState{}, err
	}
	if lastPoll != "" {
		fs.LastPoll, _ = time.Parse("2006-01-02T15:04:05Z", lastPoll)
	}
	return fs, nil
}

// SetFeedState upserts the FeedState for the given source.
func (s *Store) SetFeedState(state FeedState) error {
	lastPoll := ""
	if !state.LastPoll.IsZero() {
		lastPoll = state.LastPoll.UTC().Format("2006-01-02T15:04:05Z")
	}
	_, err := s.db.Exec(
		`INSERT INTO feed_state(source, delta_token, last_poll) VALUES(?, ?, ?)
		 ON CONFLICT(source) DO UPDATE SET delta_token=excluded.delta_token, last_poll=excluded.last_poll`,
		state.Source, state.DeltaToken, lastPoll,
	)
	return err
}

// scanFeedEvents scans rows from the feed_events table.
func scanFeedEvents(rows *sql.Rows) ([]FeedEvent, error) {
	var results []FeedEvent
	for rows.Next() {
		var fe FeedEvent
		var occurredAt string
		var cardID sql.NullInt64
		if err := rows.Scan(&fe.ID, &fe.Source, &fe.ExternalID, &fe.Payload, &occurredAt, &cardID); err != nil {
			return nil, err
		}
		fe.OccurredAt, _ = time.Parse("2006-01-02T15:04:05Z", occurredAt)
		if cardID.Valid {
			fe.CardID = cardID.Int64
		}
		results = append(results, fe)
	}
	return results, rows.Err()
}

// scanFeedCards scans rows from the feed_cards table.
func scanFeedCards(rows *sql.Rows) ([]FeedCard, error) {
	var results []FeedCard
	for rows.Next() {
		var fc FeedCard
		var oldestAt, newestAt, createdAt string
		if err := rows.Scan(
			&fc.ID, &fc.Source, &fc.SummaryMD, &fc.SummaryHTML,
			&fc.EventCount, &fc.TimeLabel, &oldestAt, &newestAt, &createdAt,
		); err != nil {
			return nil, err
		}
		fc.OldestAt, _ = time.Parse("2006-01-02T15:04:05Z", oldestAt)
		fc.NewestAt, _ = time.Parse("2006-01-02T15:04:05Z", newestAt)
		fc.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAt)
		results = append(results, fc)
	}
	return results, rows.Err()
}
