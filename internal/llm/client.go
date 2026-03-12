package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const modelsAPIBase = "https://models.inference.ai.azure.com"

// Message is a single chat turn.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// completionRequest is the OpenAI-compatible request body.
type completionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
}

// completionResponse is the OpenAI-compatible response body.
type completionResponse struct {
	Choices []struct {
		Message Message `json:"message"`
	} `json:"choices"`
}

// Client makes LLM requests via the GitHub Models API.
// Authentication is a GitHub PAT or OAuth token passed directly as a Bearer
// token — no secondary token exchange is required.
type Client struct {
	githubToken string
	model       string
	http        *http.Client
}

// NewClient creates an LLM client that authenticates with the given GitHub
// token and calls the given model.
func NewClient(githubToken, model string) *Client {
	return &Client{
		githubToken: githubToken,
		model:       model,
		http:        &http.Client{Timeout: 60 * time.Second},
	}
}

// Chat sends a conversation to the LLM and returns the assistant reply text.
func (c *Client) Chat(ctx context.Context, messages []Message) (string, error) {
	body, err := json.Marshal(completionRequest{
		Model:    c.model,
		Messages: messages,
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		modelsAPIBase+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.githubToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("chat completion: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		var errBody struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return "", fmt.Errorf("chat completion %s: %s — %s",
			resp.Status, errBody.Error.Code, errBody.Error.Message)
	}

	var cr completionResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "", fmt.Errorf("decode completion response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}
	return cr.Choices[0].Message.Content, nil
}
