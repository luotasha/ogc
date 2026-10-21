package types

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatMessageAlwaysMarshalsContent(t *testing.T) {
	for _, msg := range []ChatMessage{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call-1", Type: "function"}}},
		{Role: "tool", ToolCallID: "call-1"},
	} {
		body, err := json.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `"content":""`) {
			t.Fatalf("missing content in %s", body)
		}
	}
}
