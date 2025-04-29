package perplexity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/hashicorp/go-retryablehttp"
)

const (
	DefaultBaseURL    = "https://api.perplexity.ai"
	DefaultTimeout    = 120 * time.Second
	DefaultModel      = "sonar-reasoning-pro"
	MaxRetries        = 5
	InitialRetryDelay = 1 * time.Second
	MaxRetryDelay     = 10 * time.Second
)

// --- API Request/Response Structs ---

// Message represents a single message in the chat history.
type Message struct {
	Role    string `json:"role"` // "user", "assistant", "system"
	Content string `json:"content"`
}

// ChatCompletionRequest is the request body for the /chat/completions endpoint.
type ChatCompletionRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"` // We'll set this to false for MCP
}

// ChatCompletionChoice represents a single choice in the response.
type ChatCompletionChoice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// ChatCompletionUsage represents token usage information.
type ChatCompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatCompletionResponse is the response body from the /chat/completions endpoint.
type ChatCompletionResponse struct {
	ID      string                 `json:"id"`
	Model   string                 `json:"model"`
	Created int64                  `json:"created"` // Unix timestamp
	Usage   ChatCompletionUsage    `json:"usage"`
	Choices []ChatCompletionChoice `json:"choices"`
	// Add Error field if the API includes errors in the body
	// Error   *APIError              `json:"error,omitempty"`
}

// APIError represents a potential error returned by the Perplexity API.
// Adjust based on actual API error structure.
type APIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// --- Client Implementation ---

// Client manages communication with the Perplexity API.
type Client struct {
	httpClient *retryablehttp.Client
	apiKey     string
	baseURL    string
	model      string
}

// Config holds configuration for the Perplexity client.
type Config struct {
	APIKey  string
	BaseURL string        // Optional, defaults to DefaultBaseURL
	Timeout time.Duration // Optional, defaults to DefaultTimeout
	Model   string        // Optional, defaults to DefaultModel
}

// NewClient creates and configures a new Perplexity API client.
func NewClient(cfg Config) (*Client, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("Perplexity API key is required")
	}

	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	model := cfg.Model
	if model == "" {
		model = DefaultModel
	}

	retryClient := retryablehttp.NewClient()
	retryClient.RetryMax = MaxRetries
	retryClient.Logger = log.New(io.Discard, "", 0) // Disable default retryablehttp logging, we'll log ourselves
	retryClient.HTTPClient.Timeout = timeout      // Set overall timeout for each attempt

	// Custom retry policy similar to the TypeScript version
	retryClient.RetryWaitMin = InitialRetryDelay
	retryClient.RetryWaitMax = MaxRetryDelay
	retryClient.Backoff = retryablehttp.DefaultBackoff
	retryClient.CheckRetry = func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if err != nil {
			// Basic network errors or connection aborted
			log.Printf("Retrying due to network error: %v", err)
			return true, err
		}

		// Check for specific status codes to retry
		if resp.StatusCode == http.StatusTooManyRequests || // 429
			resp.StatusCode == http.StatusInternalServerError || // 500
			resp.StatusCode == http.StatusBadGateway || // 502
			resp.StatusCode == http.StatusServiceUnavailable || // 503
			resp.StatusCode == http.StatusGatewayTimeout { // 504
			log.Printf("Retrying due to status code %d", resp.StatusCode)
			return true, nil
		}

		// Default behavior: don't retry if response received without specific error codes
		return false, nil
	}

	// Custom error handler for logging
	retryClient.ErrorHandler = func(resp *http.Response, err error, numTries int) (*http.Response, error) {
		log.Printf("Request failed after %d attempts: %v", numTries, err)
		// Potentially inspect resp here if needed
		return resp, err
	}

	client := &Client{
		httpClient: retryClient,
		apiKey:     cfg.APIKey,
		baseURL:    baseURL,
		model:      model,
	}

	log.Println("Perplexity API client initialized.")
	return client, nil
}

// PostChatCompletions sends a request to the /chat/completions endpoint.
func (c *Client) PostChatCompletions(ctx context.Context, messages []Message) (*ChatCompletionResponse, error) {
	requestBody := ChatCompletionRequest{
		Model:    c.model,
		Messages: messages,
		Stream:   false, // MCP expects a single response
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	url := fmt.Sprintf("%s/chat/completions", c.baseURL)
	req, err := retryablehttp.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	log.Printf("Sending request to %s (model: %s, messages: %d)", url, c.model, len(messages))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Error includes retries already handled by retryablehttp
		return nil, fmt.Errorf("request failed after retries: %w", err)
	}
	defer resp.Body.Close()

	log.Printf("Received response from %s: Status %s", url, resp.Status)

	respBodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Check for non-2xx status codes that weren't retried
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("API Error: Status %d, Body: %s", resp.StatusCode, string(respBodyBytes))
		// Try to unmarshal into a standard APIError format if possible
		var apiErr APIError
		if json.Unmarshal(respBodyBytes, &apiErr) == nil && apiErr.Message != "" {
			return nil, fmt.Errorf("perplexity API error (%d): %s (%s)", resp.StatusCode, apiErr.Message, apiErr.Type)
		}
		return nil, fmt.Errorf("perplexity API error: status code %d", resp.StatusCode)
	}

	var chatResponse ChatCompletionResponse
	if err := json.Unmarshal(respBodyBytes, &chatResponse); err != nil {
		log.Printf("Failed to unmarshal successful response body: %s", string(respBodyBytes))
		return nil, fmt.Errorf("failed to unmarshal response body: %w", err)
	}

	return &chatResponse, nil
}

// Search performs a search query using the Perplexity API.
// It's currently a wrapper around PostChatCompletions.
// detailLevel is not yet used but included for future enhancement.
func (c *Client) Search(ctx context.Context, query string, detailLevel string) (string, error) {
	messages := []Message{
		{Role: "user", Content: query},
		// TODO: Potentially add a system prompt based on detailLevel
		// or use a different model if available/required for search.
	}

	resp, err := c.PostChatCompletions(ctx, messages)
	if err != nil {
		return "", fmt.Errorf("search failed: %w", err)
	}

	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("search failed: no choices returned from API")
	}

	// Return the content of the first choice
	content := resp.Choices[0].Message.Content
	log.Printf("Search successful. Query: '%s', Response Snippet: '%.50s...'", query, content)
	return content, nil
}

// TODO: Add methods for other Perplexity endpoints if needed.
