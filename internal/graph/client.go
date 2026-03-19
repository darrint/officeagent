package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
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

// SetTransport replaces the HTTP transport. Used to inject logging middleware.
func (c *Client) SetTransport(t http.RoundTripper) { c.http.Transport = t }

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
	path := fmt.Sprintf("/me/mailFolders/inbox/messages?$top=%d&$orderby=receivedDateTime+desc&$select=id,subject,receivedDateTime,bodyPreview,from", top)
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

// post performs an authenticated POST to the Graph API, sending reqBody as JSON
// and decoding the response into out. Accepts both 200 and 201 as success.
func (c *Client) post(ctx context.Context, path string, reqBody interface{}, out interface{}) error {
	tok, err := c.auth.Token(ctx)
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return fmt.Errorf("graph POST %s: %s — %s", path, body.Error.Code, body.Error.Message)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// mailFolder is a Graph mail folder with its ID and display name.
type mailFolder struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// GetOrCreateFolder returns the Graph ID of the mail folder with the given
// display name, creating it at the root if it does not exist.
func (c *Client) GetOrCreateFolder(ctx context.Context, name string) (string, error) {
	var resp listResponse[mailFolder]
	if err := c.get(ctx, "/me/mailFolders?$top=100&$select=id,displayName", &resp); err != nil {
		return "", fmt.Errorf("list mail folders: %w", err)
	}
	for _, f := range resp.Value {
		if f.DisplayName == name {
			return f.ID, nil
		}
	}
	// Not found — create it.
	var created mailFolder
	if err := c.post(ctx, "/me/mailFolders", map[string]string{"displayName": name}, &created); err != nil {
		return "", fmt.Errorf("create mail folder: %w", err)
	}
	if created.ID == "" {
		return "", fmt.Errorf("create mail folder: empty id in response")
	}
	return created.ID, nil
}

// MoveMessages moves the given message IDs to the target folder concurrently.
// Each message requires a separate POST to /me/messages/{id}/move per Graph API.
// Messages that are no longer found (404/410) are skipped gracefully — they may
// have been deleted or already moved. Returns the number of messages actually
// moved and the number skipped due to not-found responses.
func (c *Client) MoveMessages(ctx context.Context, messageIDs []string, folderID string) (moved, skipped int, err error) {
	if len(messageIDs) == 0 {
		return 0, 0, nil
	}
	type result struct {
		id       string
		err      error
		notFound bool
	}
	results := make(chan result, len(messageIDs))
	var wg sync.WaitGroup
	for _, id := range messageIDs {
		wg.Add(1)
		go func(msgID string) {
			defer wg.Done()
			path := "/me/messages/" + url.PathEscape(msgID) + "/move"
			e := c.post(ctx, path, map[string]string{"destinationId": folderID}, nil)
			notFound := e != nil && isGraphNotFound(e)
			results <- result{id: msgID, err: e, notFound: notFound}
		}(id)
	}
	wg.Wait()
	close(results)

	var errs []string
	for r := range results {
		switch {
		case r.err == nil:
			moved++
		case r.notFound:
			skipped++
			log.Printf("MoveMessages: skipping %s (not found)", r.id)
		default:
			errs = append(errs, fmt.Sprintf("%s: %v", r.id, r.err))
		}
	}
	if len(errs) > 0 {
		return moved, skipped, fmt.Errorf("move errors: %s", errs[0])
	}
	return moved, skipped, nil
}

// DriveItem is the subset of a Graph driveItem response we care about after upload.
type DriveItem struct {
	// WebURL is the browser-accessible URL for the file (requires Microsoft login).
	WebURL string `json:"webUrl"`
	// DownloadURL is a pre-authenticated download URL (~1 hour expiry, no login required).
	// It is only present in the upload response and not in subsequent GETs.
	DownloadURL string `json:"@microsoft.graph.downloadUrl"`
}

// WriteFile uploads content to the authenticated user's OneDrive at the given
// path (relative to drive root, e.g. "officeagent/briefing.html") using a
// simple PUT request. The folder is created automatically by Graph if it does
// not exist. Returns a DriveItem containing webUrl and the pre-auth
// downloadUrl (may be empty if Graph omits it).
func (c *Client) WriteFile(ctx context.Context, path, contentType string, content []byte) (DriveItem, error) {
	tok, err := c.auth.Token(ctx)
	if err != nil {
		return DriveItem{}, fmt.Errorf("get token: %w", err)
	}
	// PUT /me/drive/root:/{path}:/content
	apiPath := c.baseURL + "/me/drive/root:/" + url.PathEscape(path) + ":/content"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, apiPath, bytes.NewReader(content))
	if err != nil {
		return DriveItem{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", contentType)

	resp, err := c.http.Do(req)
	if err != nil {
		return DriveItem{}, fmt.Errorf("put file: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return DriveItem{}, fmt.Errorf("graph %s: %s — %s", resp.Status, body.Error.Code, body.Error.Message)
	}
	var item DriveItem
	if err := json.NewDecoder(resp.Body).Decode(&item); err != nil {
		return DriveItem{}, fmt.Errorf("decode drive item: %w", err)
	}
	return item, nil
}

// SendMail sends an HTML email from the authenticated user to themselves.
// The subject and htmlBody are set by the caller.
func (c *Client) SendMail(ctx context.Context, subject, htmlBody string) error {
	me, err := c.GetMe(ctx)
	if err != nil {
		return fmt.Errorf("get me: %w", err)
	}
	addr := me.Mail
	if addr == "" {
		addr = me.UPN
	}

	tok, err := c.auth.Token(ctx)
	if err != nil {
		return fmt.Errorf("get token: %w", err)
	}

	payload := struct {
		Message struct {
			Subject string `json:"subject"`
			Body    struct {
				ContentType string `json:"contentType"`
				Content     string `json:"content"`
			} `json:"body"`
			ToRecipients []struct {
				EmailAddress struct {
					Address string `json:"address"`
				} `json:"emailAddress"`
			} `json:"toRecipients"`
		} `json:"message"`
	}{}
	payload.Message.Subject = subject
	payload.Message.Body.ContentType = "HTML"
	payload.Message.Body.Content = htmlBody
	payload.Message.ToRecipients = []struct {
		EmailAddress struct {
			Address string `json:"address"`
		} `json:"emailAddress"`
	}{
		{EmailAddress: struct {
			Address string `json:"address"`
		}{Address: addr}},
	}

	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal sendMail payload: %w", err)
	}

	apiPath := c.baseURL + "/me/sendMail"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiPath, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("build sendMail request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("sendMail: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 202 Accepted is the success status for sendMail.
	if resp.StatusCode != http.StatusAccepted {
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return fmt.Errorf("graph sendMail %s: %s — %s", resp.Status, body.Error.Code, body.Error.Message)
	}
	return nil
}

// isGraphNotFound returns true if the error from a Graph API call indicates
// the resource was not found (HTTP 404 / ItemNotFound) or is gone (HTTP 410).
func isGraphNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "404") || strings.Contains(s, "ItemNotFound") ||
		strings.Contains(s, "ErrorItemNotFound") || strings.Contains(s, "410")
}
