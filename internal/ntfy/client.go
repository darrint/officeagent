// Package ntfy provides a minimal client for sending push notifications via ntfy.sh.
package ntfy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

const (
	baseURL         = "https://ntfy.sh"
	maxMessageBytes = 3800 // conservative limit; ntfy.sh converts large bodies to file attachments
)

// Send posts a Markdown-formatted message to the ntfy.sh topic.
// title is shown as the notification title.
// body is the Markdown message body; it is silently truncated to 4096 bytes
// (ntfy's limit) with a trailing indicator if it exceeds that.
// clickURL, if non-empty, is sent as the Click header so tapping the
// notification opens that URL (e.g. a pre-auth OneDrive download link).
func Send(ctx context.Context, topic, title, body, clickURL string) error {
	if topic == "" {
		return fmt.Errorf("ntfy: topic is empty")
	}
	body = truncateToLimit(body, maxMessageBytes)
	url := baseURL + "/" + topic
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("ntfy: build request: %w", err)
	}
	req.Header.Set("Title", title)
	req.Header.Set("Priority", "4")
	req.Header.Set("Markdown", "yes")
	req.Header.Set("Tags", "office,clock,summary")
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if clickURL != "" {
		req.Header.Set("Click", clickURL)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ntfy: send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("ntfy: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// truncateToLimit truncates s to at most maxBytes bytes, cutting at a valid
// UTF-8 boundary and appending "\n\n…(truncated)" if truncation occurs.
func truncateToLimit(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	suffix := "\n\n…(truncated)"
	cutAt := maxBytes - len(suffix)
	if cutAt < 0 {
		cutAt = 0
	}
	b := []byte(s)[:cutAt]
	// Walk back to the nearest valid UTF-8 boundary.
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b) + suffix
}
