package client

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/xynogen/ogc/pkg/types"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testClient(transport http.RoundTripper) *Client {
	return &Client{
		openAIConfig: EndpointConfig{BaseURL: "http://upstream.test/chat/completions", APIKey: "test-key"},
		httpClient:   &http.Client{Transport: transport},
	}
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestChatCompletionResetsSessionAndRetriesOnceOnUnauthorized(t *testing.T) {
	var mu sync.Mutex
	var sessions []string
	c := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		sessions = append(sessions, r.Header.Get("x-opencode-session"))
		n := len(sessions)
		mu.Unlock()
		if n == 1 {
			return response(http.StatusUnauthorized, `{"error":"stale session"}`), nil
		}
		return response(http.StatusOK, "data: [DONE]\n\n"), nil
	}))

	ctx := WithSession(context.Background(), http.Header{}, `{"session_id":"conversation-a"}`)
	stream := true
	resp, err := c.ChatCompletion(ctx, "model", &types.ChatCompletionRequest{Model: "model", Stream: &stream})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(sessions) != 2 {
		t.Fatalf("requests = %d, want 2", len(sessions))
	}
	if sessions[0] != "conversation-a" {
		t.Fatalf("first session = %q", sessions[0])
	}
	if !strings.HasPrefix(sessions[1], "conversation-a-reset-") {
		t.Fatalf("retry session = %q", sessions[1])
	}
}

func TestChatCompletionDoesNotRetryOtherClientErrors(t *testing.T) {
	requests := 0
	c := testClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		return response(http.StatusBadRequest, `{"error":"bad request"}`), nil
	}))

	_, err := c.ChatCompletion(context.Background(), "model", &types.ChatCompletionRequest{Model: "model"})
	if err == nil {
		t.Fatal("expected error")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}
