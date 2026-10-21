// Package client manages upstream API client connections.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/xynogen/ogc/internal/config"
	"github.com/xynogen/ogc/pkg/types"
)

// Client handles communication with upstream API.
type Client struct {
	openAIConfig    EndpointConfig
	anthropicConfig EndpointConfig
	httpClient      *http.Client
}

// EndpointConfig holds configuration for a specific API endpoint.
type EndpointConfig struct {
	BaseURL string
	APIKey  string
}

// NewClient creates a new upstream client.
func NewClient(cfg config.UpstreamConfig, apiKey string) *Client {
	timeout := time.Duration(cfg.TimeoutMs) * time.Millisecond
	if timeout == 0 {
		timeout = 5 * time.Minute
	}

	// Configure connection pooling for better performance
	transport := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		MaxConnsPerHost:     50,
		DisableKeepAlives:   false,
	}

	return &Client{
		openAIConfig: EndpointConfig{
			BaseURL: resolveURL(cfg.BaseURL, "/chat/completions"),
			APIKey:  apiKey,
		},
		anthropicConfig: EndpointConfig{
			BaseURL: resolveURL(cfg.AnthropicBaseURL, "/messages"),
			APIKey:  apiKey,
		},
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
	}
}

// ChatCompletion sends a chat completion request to the OpenAI endpoint.
// Returns the raw HTTP response for the caller to handle (streaming or body read).
func (c *Client) ChatCompletion(
	ctx context.Context,
	_ string,
	req *types.ChatCompletionRequest,
) (*http.Response, error) {
	endpoint := c.openAIConfig

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	setSessionHeaders(httpReq)

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+endpoint.APIKey)

	// Add streaming header if requested
	if req.Stream != nil && *req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	// Check for error status codes
	if resp.StatusCode >= http.StatusBadRequest {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(bodyBytes), RequestID: resp.Header.Get("x-request-id")}
	}

	return resp, nil
}

// resolveURL normalises a base URL to always end with the given path suffix.
// Accepts both base-only (http://host/v1) and already-full (http://host/v1/chat/completions).
func resolveURL(base, suffix string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, suffix) {
		return base
	}
	return base + suffix
}

// ChatCompletionNonStreaming sends a non-streaming request and returns the full parsed response.
func (c *Client) ChatCompletionNonStreaming(
	ctx context.Context,
	modelID string,
	req *types.ChatCompletionRequest,
) (*types.ChatCompletionResponse, error) {
	// Force non-streaming
	streamFalse := false
	req.Stream = &streamFalse

	resp, err := c.ChatCompletion(ctx, modelID, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	var chatResp types.ChatCompletionResponse
	if err := json.Unmarshal(body, &chatResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	return &chatResp, nil
}

// GetStreamingBody returns the response body for streaming consumption.
// The caller is responsible for closing the returned ReadCloser.
func (c *Client) GetStreamingBody(
	ctx context.Context,
	modelID string,
	req *types.ChatCompletionRequest,
) (io.ReadCloser, error) {
	// Force streaming
	streamTrue := true
	req.Stream = &streamTrue

	resp, err := c.ChatCompletion(ctx, modelID, req)
	if err != nil {
		return nil, err
	}

	return resp.Body, nil
}

// SendAnthropicRequest sends a raw Anthropic-format request (for MiniMax models).
// This skips the OpenAI transformation entirely.
func (c *Client) SendAnthropicRequest(
	ctx context.Context,
	body []byte,
	stream bool,
) (*http.Response, error) {
	endpoint := c.anthropicConfig

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.BaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	setSessionHeaders(httpReq)

	// Set headers - x-api-key is required for OpenCode Go's /v1/messages endpoint
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", endpoint.APIKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	// Add streaming header if requested
	if stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	// Check for error status codes
	if resp.StatusCode >= http.StatusBadRequest {
		bodyBytes, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, &APIError{StatusCode: resp.StatusCode, Body: string(bodyBytes), RequestID: resp.Header.Get("x-request-id")}
	}

	return resp, nil
}
