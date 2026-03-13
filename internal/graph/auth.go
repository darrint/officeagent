package graph

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"golang.org/x/oauth2"

	"github.com/darrint/officeagent/internal/store"
)

const tokenStoreKey = "graph_oauth_token"

// AuthConfig holds the Azure AD application credentials.
type AuthConfig struct {
	// ClientID is the Azure AD app registration client ID.
	// If empty, Auth will read "setting.azure_client_id" from the store.
	ClientID string
	// TenantID is the Azure AD tenant ("common" for multi-tenant).
	TenantID string
}

// Auth manages OAuth2 tokens for Microsoft Graph using the auth code + PKCE flow.
type Auth struct {
	cfg   AuthConfig
	store *store.Store
}

// graphScopes are the Microsoft Graph permissions we request.
var graphScopes = []string{
	"Mail.Read",
	"Calendars.Read",
	"offline_access",
}

// NewAuth creates an Auth from config and a token store.
func NewAuth(cfg AuthConfig, s *store.Store) *Auth {
	return &Auth{cfg: cfg, store: s}
}

// effectiveClientID returns the Azure AD client ID to use. It prefers the
// value stored in "setting.azure_client_id" (set via the Settings page) and
// falls back to the value supplied at construction time (env var).
func (a *Auth) effectiveClientID() string {
	if a.store != nil {
		if v, err := a.store.Get("setting.azure_client_id"); err == nil && v != "" {
			return v
		}
	}
	return a.cfg.ClientID
}

// oauthConfig builds an oauth2.Config using the current effective client ID.
// Called on every OAuth operation so that a Settings-page update takes effect
// without restarting the server.
func (a *Auth) oauthConfig() *oauth2.Config {
	return &oauth2.Config{
		ClientID: a.effectiveClientID(),
		Scopes:   graphScopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:   fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/authorize", a.cfg.TenantID),
			TokenURL:  fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", a.cfg.TenantID),
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
}

// AuthCodeURL generates a PKCE-protected authorization URL.
// Returns the URL to redirect the user to, the state (CSRF token), and the
// code verifier (must be kept secret and passed to ExchangeCode).
func (a *Auth) AuthCodeURL(redirectURI string) (authURL, state, verifier string, err error) {
	b := make([]byte, 16)
	if _, err = rand.Read(b); err != nil {
		return "", "", "", fmt.Errorf("generate state: %w", err)
	}
	state = base64.RawURLEncoding.EncodeToString(b)

	verifier = oauth2.GenerateVerifier()

	authURL = a.oauthConfig().AuthCodeURL(
		state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("redirect_uri", redirectURI),
	)
	return authURL, state, verifier, nil
}

// ExchangeCode exchanges an authorization code (plus PKCE verifier) for a
// token, persists it, and returns it.
func (a *Auth) ExchangeCode(ctx context.Context, code, verifier, redirectURI string) (*oauth2.Token, error) {
	tok, err := a.oauthConfig().Exchange(ctx, code,
		oauth2.VerifierOption(verifier),
		oauth2.SetAuthURLParam("redirect_uri", redirectURI),
	)
	if err != nil {
		return nil, err
	}
	if err := a.saveToken(tok); err != nil {
		return nil, fmt.Errorf("save token: %w", err)
	}
	return tok, nil
}

// Token returns a valid (possibly refreshed) token, or an error if not
// authenticated.
func (a *Auth) Token(ctx context.Context) (*oauth2.Token, error) {
	tok, err := a.loadToken()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, fmt.Errorf("not authenticated")
	}
	// Let the oauth2 library refresh if needed.
	ts := a.oauthConfig().TokenSource(ctx, tok)
	fresh, err := ts.Token()
	if err != nil {
		return nil, fmt.Errorf("refresh token: %w", err)
	}
	if fresh.AccessToken != tok.AccessToken {
		_ = a.saveToken(fresh)
	}
	return fresh, nil
}

// IsAuthenticated reports whether a valid (or refreshable) token is stored.
func (a *Auth) IsAuthenticated(ctx context.Context) bool {
	_, err := a.Token(ctx)
	return err == nil
}

func (a *Auth) saveToken(tok *oauth2.Token) error {
	b, err := json.Marshal(tok)
	if err != nil {
		return err
	}
	return a.store.Set(tokenStoreKey, string(b))
}

func (a *Auth) loadToken() (*oauth2.Token, error) {
	v, err := a.store.Get(tokenStoreKey)
	if err != nil || v == "" {
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal([]byte(v), &tok); err != nil {
		return nil, fmt.Errorf("unmarshal token: %w", err)
	}
	return &tok, nil
}
