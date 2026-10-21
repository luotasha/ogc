package transformer

import (
	"encoding/json"
	"testing"

	"github.com/xynogen/ogc/internal/config"
	"github.com/xynogen/ogc/pkg/types"
)

func TestToolResultsBeforeUserText(t *testing.T) {
	var req types.MessageRequest
	err := json.Unmarshal([]byte(`{"model":"test","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_test","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"text","text":"Continue"},{"type":"tool_result","tool_use_id":"call_test","content":"done"}]}]}`), &req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewRequestTransformer().TransformRequest(&req, config.ModelConfig{ModelID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 || got.Messages[1].Role != "tool" || got.Messages[1].ToolCallID != "call_test" || got.Messages[2].Role != "user" {
		t.Fatalf("tool result sequence invalid: %+v", got.Messages)
	}
}

func TestThinkingBlocksBecomeReasoningContent(t *testing.T) {
	var req types.MessageRequest
	err := json.Unmarshal([]byte(`{"model":"test","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"first "},{"type":"thinking","thinking":"second"},{"type":"tool_use","id":"call_test","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_test","content":"done"}]}]}`), &req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := NewRequestTransformer().TransformRequest(&req, config.ModelConfig{ModelID: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Messages[0].ReasoningContent != "first second" {
		t.Fatalf("reasoning content was lost: %+v", got.Messages[0])
	}
	body, err := json.Marshal(got)
	if err != nil || !json.Valid(body) || !containsJSONField(body, `"reasoning_content":"first second"`) {
		t.Fatalf("reasoning_content not serialized: %s (%v)", body, err)
	}
}

func containsJSONField(body []byte, field string) bool {
	for i := 0; i+len(field) <= len(body); i++ {
		if string(body[i:i+len(field)]) == field {
			return true
		}
	}
	return false
}
