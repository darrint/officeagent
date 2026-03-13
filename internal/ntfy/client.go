// Package ntfy provides a minimal client for sending push notifications via ntfy.sh.
package ntfy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const baseURL = "https://ntfy.sh"

// Send posts a Markdown-formatted message to the ntfy.sh topic.
// title is shown as the notification title.
// body is the Markdown message body.
func Send(ctx context.Context, topic, title, body string) error {
	if topic == "" {
		return fmt.Errorf("ntfy: topic is empty")
	}
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
