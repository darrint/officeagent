package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"golang.org/x/oauth2"
)

// windowsToIANA maps Windows timezone names to IANA timezone identifiers.
// Graph API returns Windows timezone names in event start/end objects.
var windowsToIANA = map[string]string{
	"Dateline Standard Time":        "Etc/GMT+12",
	"UTC-11":                        "Etc/GMT+11",
	"Hawaiian Standard Time":        "Pacific/Honolulu",
	"Alaskan Standard Time":         "America/Anchorage",
	"Pacific Standard Time":         "America/Los_Angeles",
	"Mountain Standard Time":        "America/Denver",
	"Mountain Standard Time (Mexico)": "America/Chihuahua",
	"US Mountain Standard Time":     "America/Phoenix",
	"Central Standard Time":         "America/Chicago",
	"Central Standard Time (Mexico)": "America/Mexico_City",
	"Canada Central Standard Time":  "America/Regina",
	"Eastern Standard Time":         "America/New_York",
	"US Eastern Standard Time":      "America/Indiana/Indianapolis",
	"Atlantic Standard Time":        "America/Halifax",
	"Newfoundland Standard Time":     "America/St_Johns",
	"E. South America Standard Time": "America/Sao_Paulo",
	"SA Eastern Standard Time":      "America/Cayenne",
	"UTC-02":                        "Etc/GMT+2",
	"Azores Standard Time":          "Atlantic/Azores",
	"Cape Verde Standard Time":      "Atlantic/Cape_Verde",
	"UTC":                           "UTC",
	"GMT Standard Time":             "Europe/London",
	"Greenwich Standard Time":       "Atlantic/Reykjavik",
	"W. Europe Standard Time":       "Europe/Berlin",
	"Central Europe Standard Time":  "Europe/Warsaw",
	"Romance Standard Time":         "Europe/Paris",
	"Central European Standard Time": "Europe/Warsaw",
	"W. Central Africa Standard Time": "Africa/Lagos",
	"Jordan Standard Time":          "Asia/Amman",
	"GTB Standard Time":             "Europe/Bucharest",
	"Middle East Standard Time":     "Asia/Beirut",
	"Egypt Standard Time":           "Africa/Cairo",
	"E. Europe Standard Time":       "Asia/Nicosia",
	"FLE Standard Time":             "Europe/Kiev",
	"Turkey Standard Time":          "Europe/Istanbul",
	"Israel Standard Time":          "Asia/Jerusalem",
	"Arabic Standard Time":          "Asia/Baghdad",
	"Arab Standard Time":            "Asia/Riyadh",
	"E. Africa Standard Time":       "Africa/Nairobi",
	"Iran Standard Time":            "Asia/Tehran",
	"Arabian Standard Time":         "Asia/Dubai",
	"Azerbaijan Standard Time":      "Asia/Baku",
	"Mauritius Standard Time":       "Indian/Mauritius",
	"Caucasus Standard Time":        "Asia/Yerevan",
	"Afghanistan Standard Time":     "Asia/Kabul",
	"Pakistan Standard Time":        "Asia/Karachi",
	"India Standard Time":           "Asia/Calcutta",
	"Sri Lanka Standard Time":       "Asia/Colombo",
	"Nepal Standard Time":           "Asia/Katmandu",
	"Central Asia Standard Time":    "Asia/Almaty",
	"Bangladesh Standard Time":      "Asia/Dhaka",
	"Myanmar Standard Time":         "Asia/Rangoon",
	"SE Asia Standard Time":         "Asia/Bangkok",
	"China Standard Time":           "Asia/Shanghai",
	"Singapore Standard Time":       "Asia/Singapore",
	"W. Australia Standard Time":    "Australia/Perth",
	"Taipei Standard Time":          "Asia/Taipei",
	"Tokyo Standard Time":           "Asia/Tokyo",
	"Korea Standard Time":           "Asia/Seoul",
	"Cen. Australia Standard Time":  "Australia/Adelaide",
	"AUS Central Standard Time":     "Australia/Darwin",
	"E. Australia Standard Time":    "Australia/Brisbane",
	"AUS Eastern Standard Time":     "Australia/Sydney",
	"West Pacific Standard Time":    "Pacific/Port_Moresby",
	"Tasmania Standard Time":        "Australia/Hobart",
	"Magadan Standard Time":         "Asia/Magadan",
	"Central Pacific Standard Time": "Pacific/Guadalcanal",
	"New Zealand Standard Time":     "Pacific/Auckland",
	"Fiji Standard Time":            "Pacific/Fiji",
	"Tonga Standard Time":           "Pacific/Tongatapu",
}

// parseEventTime parses a Graph API event time string with its associated
// Windows or IANA timezone name. Falls back to UTC if the timezone is unknown.
func parseEventTime(dateStr, tzName string) time.Time {
	loc := resolveLocation(tzName)
	// Graph returns times without fractional seconds in some cases; try both formats.
	for _, layout := range []string{"2006-01-02T15:04:05.0000000", "2006-01-02T15:04:05"} {
		t, err := time.ParseInLocation(layout, dateStr, loc)
		if err == nil {
			return t
		}
	}
	log.Printf("graph: failed to parse event time %q (tz %q), using zero time", dateStr, tzName)
	return time.Time{}
}

// resolveLocation returns the *time.Location for tzName, trying IANA names
// first, then the Windows-to-IANA map, then UTC.
func resolveLocation(tzName string) *time.Location {
	if tzName == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(tzName); err == nil {
		return loc
	}
	if iana, ok := windowsToIANA[tzName]; ok {
		if loc, err := time.LoadLocation(iana); err == nil {
			return loc
		}
	}
	log.Printf("graph: unknown timezone %q, falling back to UTC", tzName)
	return time.UTC
}

const graphBase = "https://graph.microsoft.com/v1.0"

// tokenProvider is satisfied by *Auth and by test fakes.
type tokenProvider interface {
	Token(ctx context.Context) (*oauth2.Token, error)
}

// Client makes authenticated requests to Microsoft Graph.
type Client struct {
	auth    tokenProvider
	http    *http.Client
	baseURL string // defaults to graphBase; overridable in tests
}

// NewClient creates a Graph API client backed by the given Auth.
func NewClient(auth *Auth) *Client {
	return &Client{
		auth:    auth,
		http:    &http.Client{Timeout: 30 * time.Second},
		baseURL: graphBase,
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

// SetBaseURL overrides the API base URL. Intended for testing only.
func (c *Client) SetBaseURL(u string) { c.baseURL = u }

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

// ListEvents returns upcoming calendar events, including recurring event instances.
func (c *Client) ListEvents(ctx context.Context, top int) ([]Event, error) {
	var resp listResponse[graphEvent]
	now := time.Now().UTC().Format(time.RFC3339)
	// Use $orderby=start/dateTime to guarantee chronological order and ensure
	// recurring event instances are returned (not only series masters).
	// Prefer: outlook.timezone="UTC" makes Graph interpret the window boundaries
	// in UTC, matching our startDateTime/endDateTime values.
	path := fmt.Sprintf(
		"/me/calendarview?startDateTime=%s&endDateTime=%s&$top=%d&$orderby=start/dateTime&$select=id,subject,start,end",
		now,
		time.Now().UTC().Add(30*24*time.Hour).Format(time.RFC3339),
		top,
	)
	if err := c.get(ctx, path, &resp, map[string]string{
		"Prefer": `outlook.timezone="UTC"`,
	}); err != nil {
		return nil, err
	}
	events := make([]Event, len(resp.Value))
	for i, e := range resp.Value {
		start := parseEventTime(e.Start.DateTime, e.Start.TimeZone)
		end := parseEventTime(e.End.DateTime, e.End.TimeZone)
		events[i] = Event{
			ID:      e.ID,
			Subject: e.Subject,
			Start:   start,
			End:     end,
		}
	}
	return events, nil
}

// User is a simplified representation of a Microsoft Graph user profile.
type User struct {
	DisplayName string `json:"displayName"`
	Mail        string `json:"mail"`
	UPN         string `json:"userPrincipalName"`
}

// GetMe returns the authenticated user's profile from /me.
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	var u User
	if err := c.get(ctx, "/me?$select=displayName,mail,userPrincipalName", &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// get performs an authenticated GET request to the Graph API and decodes the
// JSON response into out. Optional extraHeaders maps are merged into the request.
func (c *Client) get(ctx context.Context, path string, out interface{}, extraHeaders ...map[string]string) error {
	tok, err := c.auth.Token(ctx)
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Accept", "application/json")
	for _, h := range extraHeaders {
		for k, v := range h {
			req.Header.Set(k, v)
		}
	}

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
