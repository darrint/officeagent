// Package fastmail provides a minimal JMAP client for reading Fastmail inbox
// messages. It uses an API token (not OAuth) which the user generates at
// https://app.fastmail.com/settings/security/tokens.
package fastmail

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const (
	sessionURL = "https://api.fastmail.com/jmap/session"
	mailCap    = "urn:ietf:params:jmap:mail"
	coreCap    = "urn:ietf:params:jmap:core"
)

// Client is a minimal Fastmail JMAP client.
type Client struct {
	token   string
	httpCli *http.Client
	// baseURL overrides the JMAP session URL; used in tests.
	baseURL string
}

// NewClient creates a Fastmail JMAP client authenticated with an API token.
func NewClient(token string) *Client {
	return &Client{
		token:   token,
		httpCli: &http.Client{Timeout: 30 * time.Second},
		baseURL: sessionURL,
	}
}

// Message is a simplified mail message returned by ListMessages.
type Message struct {
	From        string
	Subject     string
	ReceivedAt  time.Time
	BodyPreview string
}

// jmapSession is the subset of the JMAP session resource we need.
type jmapSession struct {
	APIURL          string            `json:"apiUrl"`
	PrimaryAccounts map[string]string `json:"primaryAccounts"`
}

// getSession fetches the JMAP session resource.
func (c *Client) getSession(ctx context.Context) (*jmapSession, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("session request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("session request: HTTP %s", resp.Status)
	}
	var s jmapSession
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		return nil, fmt.Errorf("decode session: %w", err)
	}
	if s.APIURL == "" {
		return nil, fmt.Errorf("session missing apiUrl")
	}
	return &s, nil
}

// jmapCall sends a batch of JMAP method calls and returns raw method responses.
func (c *Client) jmapCall(ctx context.Context, apiURL string, calls []interface{}) ([]json.RawMessage, error) {
	body, err := json.Marshal(map[string]interface{}{
		"using":       []string{coreCap, mailCap},
		"methodCalls": calls,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jmap call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jmap call: HTTP %s", resp.Status)
	}
	var out struct {
		MethodResponses []json.RawMessage `json:"methodResponses"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode jmap response: %w", err)
	}
	return out.MethodResponses, nil
}

// getInboxID returns the JMAP ID of the primary inbox mailbox.
func (c *Client) getInboxID(ctx context.Context, sess *jmapSession) (string, error) {
	accountID := sess.PrimaryAccounts[mailCap]
	if accountID == "" {
		return "", fmt.Errorf("no mail account in session")
	}

	resps, err := c.jmapCall(ctx, sess.APIURL, []interface{}{
		[]interface{}{
			"Mailbox/query",
			map[string]interface{}{
				"accountId": accountID,
				"filter":    map[string]interface{}{"role": "inbox"},
			},
			"0",
		},
	})
	if err != nil {
		return "", err
	}
	if len(resps) == 0 {
		return "", fmt.Errorf("no method responses for Mailbox/query")
	}

	// methodResponse = [name, args, callId]
	var tuple []json.RawMessage
	if err := json.Unmarshal(resps[0], &tuple); err != nil || len(tuple) < 2 {
		return "", fmt.Errorf("malformed Mailbox/query response")
	}
	var args struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(tuple[1], &args); err != nil {
		return "", fmt.Errorf("decode Mailbox/query args: %w", err)
	}
	if len(args.IDs) == 0 {
		return "", fmt.Errorf("inbox mailbox not found")
	}
	return args.IDs[0], nil
}

// ListMessages returns the top n most recently received inbox messages.
func (c *Client) ListMessages(ctx context.Context, top int) ([]Message, error) {
	sess, err := c.getSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	inboxID, err := c.getInboxID(ctx, sess)
	if err != nil {
		return nil, fmt.Errorf("get inbox: %w", err)
	}

	accountID := sess.PrimaryAccounts[mailCap]

	// Single JMAP request: Email/query to find IDs, Email/get for details.
	resps, err := c.jmapCall(ctx, sess.APIURL, []interface{}{
		// Call 0: find email IDs in inbox, newest first
		[]interface{}{
			"Email/query",
			map[string]interface{}{
				"accountId": accountID,
				"filter":    map[string]interface{}{"inMailbox": inboxID},
				"sort":      []map[string]interface{}{{"property": "receivedAt", "isAscending": false}},
				"limit":     top,
			},
			"0",
		},
		// Call 1: fetch email details using back-reference to call 0 ids
		[]interface{}{
			"Email/get",
			map[string]interface{}{
				"accountId": accountID,
				"#ids": map[string]interface{}{
					"name":     "0",
					"path":     "/ids",
					"resultOf": "0",
				},
				"properties": []string{"from", "subject", "receivedAt", "preview"},
			},
			"1",
		},
	})
	if err != nil {
		return nil, err
	}
	if len(resps) < 2 {
		return nil, fmt.Errorf("expected 2 method responses, got %d", len(resps))
	}

	// Parse the Email/get response (index 1).
	var tuple []json.RawMessage
	if err := json.Unmarshal(resps[1], &tuple); err != nil || len(tuple) < 2 {
		return nil, fmt.Errorf("malformed Email/get response")
	}
	var emailGetArgs struct {
		List []struct {
			Subject    string    `json:"subject"`
			Preview    string    `json:"preview"`
			ReceivedAt time.Time `json:"receivedAt"`
			From       []struct {
				Name  string `json:"name"`
				Email string `json:"email"`
			} `json:"from"`
		} `json:"list"`
	}
	if err := json.Unmarshal(tuple[1], &emailGetArgs); err != nil {
		return nil, fmt.Errorf("decode Email/get args: %w", err)
	}

	msgs := make([]Message, 0, len(emailGetArgs.List))
	for _, e := range emailGetArgs.List {
		from := ""
		if len(e.From) > 0 {
			if e.From[0].Name != "" {
				from = e.From[0].Name + " <" + e.From[0].Email + ">"
			} else {
				from = e.From[0].Email
			}
		}
		msgs = append(msgs, Message{
			From:        from,
			Subject:     e.Subject,
			ReceivedAt:  e.ReceivedAt,
			BodyPreview: e.Preview,
		})
	}
	return msgs, nil
}
