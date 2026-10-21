package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureAndPatchNonStreaming(t *testing.T) {
	p := testProxy(t)
	p.captureResponse([]byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"think fully","tool_calls":[{"id":"call_1","type":"function"}]}}]}`))
	body, n := p.patchRequest([]byte(`{"messages":[{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function"}]}]}`))
	if n != 1 || !strings.Contains(string(body), `"reasoning_content":"think fully"`) {
		t.Fatalf("not patched: n=%d body=%s", n, body)
	}
}

func TestCaptureStreamingReasoning(t *testing.T) {
	p := testProxy(t)
	var reasoning strings.Builder
	toolCalls := newStreamToolCalls()
	captureStreamLine(`data: {"choices":[{"delta":{"reasoning_content":"think "}}]}`, &reasoning, toolCalls)
	captureStreamLine(`data: {"choices":[{"delta":{"reasoning_content":"fully","tool_calls":[{"id":"call_","index":0}]}}]}`, &reasoning, toolCalls)
	captureStreamLine(`data: {"choices":[{"delta":{"tool_calls":[{"id":"2","index":0}]}}]}`, &reasoning, toolCalls)
	p.cache.put(toolCalls.ids(), reasoning.String())
	body, n := p.patchRequest([]byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_2"}]}]}`))
	if n != 1 {
		t.Fatalf("expected patch: %s", body)
	}
	var root map[string]any
	_ = json.Unmarshal(body, &root)
	msgs := root["messages"].([]any)
	if got := msgs[0].(map[string]any)["reasoning_content"]; got != "think fully" {
		t.Fatalf("got %v", got)
	}
	if got, ok := msgs[0].(map[string]any)["content"]; !ok || got != "" {
		t.Fatalf("missing normalized content: %v", msgs[0])
	}
}

func TestStreamingToolCallIDsAreReassembledByIndex(t *testing.T) {
	var reasoning strings.Builder
	toolCalls := newStreamToolCalls()
	captureStreamLine(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ab"},{"index":1,"id":"call_x"}]}}]}`, &reasoning, toolCalls)
	captureStreamLine(`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"cd"},{"index":1,"id":"yz"}]}}]}`, &reasoning, toolCalls)
	got := toolCalls.ids()
	if len(got) != 2 || got[0] != "call_abcd" || got[1] != "call_xyz" {
		t.Fatalf("unexpected IDs: %#v", got)
	}
}

func TestPatchAddsContentWithoutCachedReasoning(t *testing.T) {
	p := testProxy(t)
	body, n := p.patchRequest([]byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"unknown"}]},{"role":"tool","tool_call_id":"unknown"}]}`))
	if n != 1 {
		t.Fatalf("expected compatibility reasoning injection: %d", n)
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	msg := root["messages"].([]any)[0].(map[string]any)
	if got, ok := msg["content"]; !ok || got != "" {
		t.Fatalf("content not normalized: %v", msg)
	}
	if got := msg["reasoning_content"]; got != missingReasoningFallback {
		t.Fatalf("missing fallback reasoning: %v", got)
	}
	tool := root["messages"].([]any)[1].(map[string]any)
	if got, ok := tool["content"]; !ok || got != "" {
		t.Fatalf("tool content not normalized: %v", tool)
	}
}

func TestForwardStreamPublishesOnlyCompleteReasoning(t *testing.T) {
	p := testProxy(t)
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"think ","tool_calls":[{"id":"call_race","index":0}]}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"fully"}}]}`,
	}, "\n") + "\n"
	p.forwardStream(httptest.NewRecorder(), strings.NewReader(stream))
	if _, ok := p.cache.get([]string{"call_race"}); ok {
		t.Fatal("published reasoning before the stream completed")
	}

	stream += "data: [DONE]\n"
	p.forwardStream(httptest.NewRecorder(), strings.NewReader(stream))
	if got, ok := p.cache.get([]string{"call_race"}); !ok || got != "think fully" {
		t.Fatalf("complete reasoning not published: %q %v", got, ok)
	}
}

func TestStreamCompleteLine(t *testing.T) {
	for _, line := range []string{
		"data: [DONE]",
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	} {
		if !streamCompleteLine(line) {
			t.Fatalf("expected complete: %s", line)
		}
	}
	if streamCompleteLine(`data: {"choices":[{"delta":{"reasoning_content":"partial"}}]}`) {
		t.Fatal("partial chunk marked complete")
	}
}

func TestCachePersists(t *testing.T) {
	p := testProxy(t)
	p.cache.put([]string{"call_3"}, "secret reasoning")
	loaded := loadCache(p.cache.path)
	if got, ok := loaded.get([]string{"call_3"}); !ok || got != "secret reasoning" {
		t.Fatalf("cache missing: %q %v", got, ok)
	}
}

func testProxy(t *testing.T) *proxy {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "cache.json")
	t.Cleanup(func() { _ = os.Remove(path) })
	return &proxy{cache: loadCache(path)}
}
