// Package feed defines shared types used by the background poller and agents
// for the event timeline feed.
package feed

import "time"

// RawEvent is returned by each agent's Poll method. It represents a single
// discrete event (email, calendar item, PR, etc.) from the source API.
// The Payload field is a JSON blob with source-specific fields; its structure
// is determined by the producing agent and consumed by the LLM summarizer.
type RawEvent struct {
	// Source identifies the originating service, e.g. "graph_mail",
	// "graph_calendar", "github", "fastmail".
	Source string

	// ExternalID is the stable, unique identifier for this event in the
	// source API. Used for deduplication (INSERT OR IGNORE).
	ExternalID string

	// Payload is a JSON-encoded object with source-specific fields.
	Payload string

	// OccurredAt is the event's own timestamp from the source API (e.g.
	// receivedDateTime for mail, start.dateTime for calendar).
	OccurredAt time.Time
}
