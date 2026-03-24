// Package poller implements the background feed event poller.
// It runs on a 15-minute ticker, calling Poll on each configured agent,
// storing new events in the database, and publishing notification counts
// to the feedBus when new events arrive.
package poller

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/darrint/officeagent/internal/feed"
	"github.com/darrint/officeagent/internal/store"
)

// Agent is the interface each feed source must implement to participate in
// background polling.
type Agent interface {
	// Poll fetches new events since the last poll and returns them as
	// RawEvents. It is responsible for reading and updating the
	// feed_state cursor in the store. Returning nil, nil is valid when
	// there are no new events or the agent is not configured.
	Poll(ctx context.Context, s *store.Store) ([]feed.RawEvent, error)
}

// FeedBus receives new-event notifications from the poller.
// Implementations are expected to be safe for concurrent use.
type FeedBus interface {
	// Publish sends a map of source → new-event-count to all connected
	// SSE clients. Only called when at least one event was inserted.
	Publish(counts map[string]int)
}

// Poller runs periodic feed collection.
type Poller struct {
	agents   []Agent
	store    *store.Store
	bus      FeedBus
	interval time.Duration

	cancelMu sync.Mutex
	cancel   context.CancelFunc
}

// New creates a Poller with the given agents, store, bus, and tick interval.
// Pass a zero interval to use the default 15-minute interval.
func New(agents []Agent, s *store.Store, bus FeedBus, interval time.Duration) *Poller {
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	return &Poller{
		agents:   agents,
		store:    s,
		bus:      bus,
		interval: interval,
	}
}

// Start launches the polling loop in a background goroutine. The loop stops
// when ctx is cancelled. Start is idempotent: calling it a second time cancels
// the previous loop and starts a fresh one.
func (p *Poller) Start(ctx context.Context) {
	newCtx, cancel := context.WithCancel(ctx)

	p.cancelMu.Lock()
	if p.cancel != nil {
		p.cancel() // stop any previous loop
	}
	p.cancel = cancel
	p.cancelMu.Unlock()

	go p.run(newCtx)
}

// Stop cancels the background polling loop.
func (p *Poller) Stop() {
	p.cancelMu.Lock()
	defer p.cancelMu.Unlock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
}

// RunOnce executes a single poll cycle synchronously. This is the same logic
// used by the background loop; it is exported primarily for testing and for
// triggering an immediate poll on startup.
func (p *Poller) RunOnce(ctx context.Context) {
	counts := make(map[string]int)

	for _, ag := range p.agents {
		events, err := ag.Poll(ctx, p.store)
		if err != nil {
			log.Printf("poller: agent poll error: %v", err)
			continue
		}
		for _, ev := range events {
			inserted, err := p.store.SaveFeedEvent(ev.Source, ev.ExternalID, ev.Payload, ev.OccurredAt)
			if err != nil {
				log.Printf("poller: save event %s/%s: %v", ev.Source, ev.ExternalID, err)
				continue
			}
			if inserted {
				counts[ev.Source]++
			}
		}
	}

	if len(counts) > 0 && p.bus != nil {
		p.bus.Publish(counts)
	}
}

func (p *Poller) run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.RunOnce(ctx)
		}
	}
}
