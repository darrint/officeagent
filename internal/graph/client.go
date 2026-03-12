package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const graphBase = "https://graph.microsoft.com/v1.0"

// Client makes authenticated requests to Microsoft Graph.
type Client struct {
	auth *Auth
	http *http.Client
}

// NewClient creates a Graph API client backed by the given Auth.
func NewClient(auth *Auth) *Client {
	return &Client{
		auth: auth,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// Message is a simplified representation of a mail message.
type Message struct {
	ID          string    `json:"id"`
	Subject     string    `json:"subject"`
	ReceivedAt  time.Time `json:"receivedDateTime"`
	BodyPreview string    `json:"bodyPreview"`
	From        string    `json:"-"` // populated from nested JSON
}

// Event is a simplified calendar event.
type Event struct {
	ID      string    `json:"id"`
	Subject string    `json:"subject"`
	Start   time.Time `json:"-"` // populated from nested JSON
	End     time.Time `json:"-"` // populated from nested JSON
}

// graphMessage is the raw JSON shape returned by Graph for messages.
type graphMessage struct {
	ID          string    `json:"id"`
	Subject     string    `json:"subject"`
	ReceivedAt  time.Time `json:"receivedDateTime"`
	BodyPreview string    `json:"bodyPreview"`
	From        struct {
		EmailAddress struct {
			Name    string `json:"name"`
			Address string `json:"address"`
		} `json:"emailAddress"`
	} `json:"from"`
}

// graphEvent is the raw JSON shape returned by Graph for calendar events.
type graphEvent struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Start   struct {
		DateTime string `json:"dateTime"`
		TimeZone string `json:"timeZone"`
	} `json:"start"`
	End struct {
		DateTime string `json:"dateTime"`
		TimeZone string `json:"timeZone"`
	} `json:"end"`
}

type listResponse[T any] struct {
	Value []T `json:"value"`
}

// ListMessages returns the top n most recent inbox messages.
func (c *Client) ListMessages(ctx context.Context, top int) ([]Message, error) {
	var resp listResponse[graphMessage]
	path := fmt.Sprintf("/me/messages?$top=%d&$orderby=receivedDateTime+desc&$select=id,subject,receivedDateTime,bodyPreview,from", top)
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	msgs := make([]Message, len(resp.Value))
	for i, m := range resp.Value {
		msgs[i] = Message{
			ID:          m.ID,
			Subject:     m.Subject,
			ReceivedAt:  m.ReceivedAt,
			BodyPreview: m.BodyPreview,
			From:        m.From.EmailAddress.Name + " <" + m.From.EmailAddress.Address + ">",
		}
	}
	return msgs, nil
}

// ListEvents returns upcoming calendar events.
func (c *Client) ListEvents(ctx context.Context, top int) ([]Event, error) {
	var resp listResponse[graphEvent]
	now := time.Now().UTC().Format(time.RFC3339)
	path := fmt.Sprintf("/me/calendarview?startDateTime=%s&endDateTime=%s&$top=%d&$select=id,subject,start,end",
		now,
		time.Now().UTC().Add(30*24*time.Hour).Format(time.RFC3339),
		top,
	)
	if err := c.get(ctx, path, &resp); err != nil {
		return nil, err
	}
	events := make([]Event, len(resp.Value))
	for i, e := range resp.Value {
		start, _ := time.Parse("2006-01-02T15:04:05.0000000", e.Start.DateTime)
		end, _ := time.Parse("2006-01-02T15:04:05.0000000", e.End.DateTime)
		events[i] = Event{
			ID:      e.ID,
			Subject: e.Subject,
			Start:   start,
			End:     end,
		}
	}
	return events, nil
}

// get performs an authenticated GET request to the Graph API and decodes the
// JSON response into out.
func (c *Client) get(ctx context.Context, path string, out interface{}) error {
	tok, err := c.auth.Token(ctx)
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, graphBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return fmt.Errorf("graph %s: %s — %s", resp.Status, body.Error.Code, body.Error.Message)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
