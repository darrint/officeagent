package config

import "os"

// Config holds the application configuration.
type Config struct {
	// Addr is the TCP address the HTTP server listens on.
	Addr string

	// DBPath is the path to the SQLite database file.
	DBPath string

	// AzureClientID is the Azure AD app registration client ID.
	// Required for Microsoft Graph API access.
	// Set via OFFICEAGENT_CLIENT_ID environment variable.
	AzureClientID string

	// AzureTenantID is the Azure AD tenant ID.
	// Defaults to "common" (multi-tenant / personal accounts).
	// Set via OFFICEAGENT_TENANT_ID environment variable.
	AzureTenantID string

	// RedirectURI is the OAuth2 callback URL registered in Azure AD.
	// Must match exactly what is registered in the app registration.
	// Set via OFFICEAGENT_REDIRECT_URI environment variable.
	// Defaults to http://localhost:8080/login/callback.
	RedirectURI string

	// GitHubToken is a GitHub personal access token (or OAuth token) used to
	// obtain a short-lived GitHub Copilot API token.
	// Set via GITHUB_TOKEN environment variable.
	GitHubToken string

	// LLMModel is the model identifier passed to the LLM completions API.
	// Set via OFFICEAGENT_LLM_MODEL environment variable.
	// Defaults to claude-sonnet-4.6.
	LLMModel string
}

// Default returns a Config populated with defaults and environment overrides.
func Default() *Config {
	tenantID := os.Getenv("OFFICEAGENT_TENANT_ID")
	if tenantID == "" {
		tenantID = "common"
	}
	redirectURI := os.Getenv("OFFICEAGENT_REDIRECT_URI")
	if redirectURI == "" {
		redirectURI = "http://localhost:8080/login/callback"
	}
	llmModel := os.Getenv("OFFICEAGENT_LLM_MODEL")
	if llmModel == "" {
		llmModel = "claude-sonnet-4.6"
	}
	return &Config{
		Addr:          "127.0.0.1:8080",
		DBPath:        "officeagent.db",
		AzureClientID: os.Getenv("OFFICEAGENT_CLIENT_ID"),
		AzureTenantID: tenantID,
		RedirectURI:   redirectURI,
		GitHubToken:   os.Getenv("GITHUB_TOKEN"),
		LLMModel:      llmModel,
	}
}
