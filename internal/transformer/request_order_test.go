package transformer
import("encoding/json";"testing";"github.com/xynogen/ogc/pkg/types";"github.com/xynogen/ogc/internal/config")
func TestToolResultsBeforeUserText(t *testing.T){
 var req types.MessageRequest
 err:=json.Unmarshal([]byte(`{"model":"test","max_tokens":64,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_test","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"text","text":"Continue"},{"type":"tool_result","tool_use_id":"call_test","content":"done"}]}]}`),&req)
 if err!=nil {t.Fatal(err)}
 got,err:=NewRequestTransformer().TransformRequest(&req,config.ModelConfig{ModelID:"test"});if err!=nil{t.Fatal(err)}
 if len(got.Messages)!=3 || got.Messages[1].Role!="tool" || got.Messages[1].ToolCallID!="call_test" || got.Messages[2].Role!="user"{t.Fatalf("tool result sequence invalid: %+v",got.Messages)}
}
