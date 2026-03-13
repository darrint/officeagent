package store_test

import (
	"testing"

	"github.com/darrint/officeagent/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(":memory:")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestGet_missingKey(t *testing.T) {
	s := newTestStore(t)
	v, err := s.Get("nonexistent")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v != "" {
		t.Fatalf("expected empty string, got %q", v)
	}
}

func TestSet_Get_roundtrip(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set("key", "value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := s.Get("key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v != "value" {
		t.Fatalf("expected %q, got %q", "value", v)
	}
}

func TestSet_overwrite(t *testing.T) {
	s := newTestStore(t)
	if err := s.Set("key", "first"); err != nil {
		t.Fatalf("Set first: %v", err)
	}
	if err := s.Set("key", "second"); err != nil {
		t.Fatalf("Set second: %v", err)
	}
	v, err := s.Get("key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v != "second" {
		t.Fatalf("expected %q after overwrite, got %q", "second", v)
	}
}

func TestSet_Get_multipleKeys(t *testing.T) {
	s := newTestStore(t)
	pairs := [][2]string{{"a", "1"}, {"b", "2"}, {"c", "3"}}
	for _, p := range pairs {
		if err := s.Set(p[0], p[1]); err != nil {
			t.Fatalf("Set %q: %v", p[0], err)
		}
	}
	for _, p := range pairs {
		v, err := s.Get(p[0])
		if err != nil {
			t.Fatalf("Get %q: %v", p[0], err)
		}
		if v != p[1] {
			t.Fatalf("key %q: expected %q, got %q", p[0], p[1], v)
		}
	}
}

func TestAddFeedback_RecentFeedback_order(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddFeedback("email", "good", "first"); err != nil {
		t.Fatalf("AddFeedback: %v", err)
	}
	if err := s.AddFeedback("email", "bad", "second"); err != nil {
		t.Fatalf("AddFeedback: %v", err)
	}
	entries, err := s.RecentFeedback("email", 10)
	if err != nil {
		t.Fatalf("RecentFeedback: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	// newest first
	if entries[0].Note != "second" {
		t.Errorf("expected newest entry first, got note %q", entries[0].Note)
	}
	if entries[0].Rating != "bad" {
		t.Errorf("expected rating %q, got %q", "bad", entries[0].Rating)
	}
	if entries[1].Note != "first" {
		t.Errorf("expected older entry second, got note %q", entries[1].Note)
	}
}

func TestRecentFeedback_limit(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		if err := s.AddFeedback("calendar", "good", ""); err != nil {
			t.Fatalf("AddFeedback: %v", err)
		}
	}
	entries, err := s.RecentFeedback("calendar", 3)
	if err != nil {
		t.Fatalf("RecentFeedback: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries (limit), got %d", len(entries))
	}
}

func TestRecentFeedback_sectionFilter(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddFeedback("email", "good", "email entry"); err != nil {
		t.Fatalf("AddFeedback email: %v", err)
	}
	if err := s.AddFeedback("calendar", "bad", "calendar entry"); err != nil {
		t.Fatalf("AddFeedback calendar: %v", err)
	}

	emailEntries, err := s.RecentFeedback("email", 10)
	if err != nil {
		t.Fatalf("RecentFeedback email: %v", err)
	}
	if len(emailEntries) != 1 {
		t.Fatalf("expected 1 email entry, got %d", len(emailEntries))
	}
	if emailEntries[0].Section != "email" {
		t.Errorf("expected section %q, got %q", "email", emailEntries[0].Section)
	}

	calEntries, err := s.RecentFeedback("calendar", 10)
	if err != nil {
		t.Fatalf("RecentFeedback calendar: %v", err)
	}
	if len(calEntries) != 1 {
		t.Fatalf("expected 1 calendar entry, got %d", len(calEntries))
	}
	if calEntries[0].Section != "calendar" {
		t.Errorf("expected section %q, got %q", "calendar", calEntries[0].Section)
	}
}

func TestRecentFeedback_empty(t *testing.T) {
	s := newTestStore(t)
	entries, err := s.RecentFeedback("email", 10)
	if err != nil {
		t.Fatalf("RecentFeedback: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries for empty store, got %d", len(entries))
	}
}

func TestFeedback_fields(t *testing.T) {
	s := newTestStore(t)
	if err := s.AddFeedback("calendar", "bad", "needs more detail"); err != nil {
		t.Fatalf("AddFeedback: %v", err)
	}
	entries, err := s.RecentFeedback("calendar", 1)
	if err != nil {
		t.Fatalf("RecentFeedback: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	f := entries[0]
	if f.Section != "calendar" {
		t.Errorf("Section: expected %q, got %q", "calendar", f.Section)
	}
	if f.Rating != "bad" {
		t.Errorf("Rating: expected %q, got %q", "bad", f.Rating)
	}
	if f.Note != "needs more detail" {
		t.Errorf("Note: expected %q, got %q", "needs more detail", f.Note)
	}
	if f.ID == 0 {
		t.Errorf("ID should be non-zero")
	}
	if f.CreatedAt.IsZero() {
		t.Errorf("CreatedAt should be non-zero")
	}
}
