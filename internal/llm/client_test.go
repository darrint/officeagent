package llm_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/darrint/officeagent/internal/llm"
)

// newTestClient creates an LLM client wired to the given test server.
func newTestClient(t *testing.T, ts *httptest.Server) *llm.Client {
	t.Helper()
	c := llm.NewClient("test-token", "test-model")
	c.SetBaseURL(ts.URL)
	return c
}

func TestChat_success(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("unexpected Authorization header: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "Hello, world!"}},
			},
		})
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	reply, err := c.Chat(context.Background(), []llm.Message{
		{Role: "user", Content: "Hi"},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if reply != "Hello, world!" {
		t.Errorf("expected %q, got %q", "Hello, world!", reply)
	}
}

func TestChat_requestBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if req.Model != "test-model" {
			t.Errorf("model: expected %q, got %q", "test-model", req.Model)
		}
		if len(req.Messages) != 2 {
			t.Errorf("expected 2 messages, got %d", len(req.Messages))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"role": "assistant", "content": "ok"}},
			},
		})
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	_, err := c.Chat(context.Background(), []llm.Message{
		{Role: "system", Content: "You are helpful."},
		{Role: "user", Content: "Hello"},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
}

func TestChat_httpError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{
				"code":    "Unauthorized",
				"message": "Invalid token",
			},
		})
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	_, err := c.Chat(context.Background(), []llm.Message{{Role: "user", Content: "Hi"}})
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("expected 401 in error, got: %v", err)
	}
}

func TestChat_noChoices(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{}})
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	_, err := c.Chat(context.Background(), []llm.Message{{Role: "user", Content: "Hi"}})
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("expected 'no choices' in error, got: %v", err)
	}
}

func TestChat_malformedJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("not json{{{"))
	}))
	defer ts.Close()

	c := newTestClient(t, ts)
	_, err := c.Chat(context.Background(), []llm.Message{{Role: "user", Content: "Hi"}})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}
