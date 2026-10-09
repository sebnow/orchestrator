package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenAgentRequestsWhenMarshallingThenTheyHaveTheWireFieldNames(t *testing.T) {
	for want, value := range map[string]any{
		`{"kind":"spawn","payload":{"prompt":"Say PEAR.","model":"haiku"}}`: protocol.AgentRequest{Kind: protocol.AgentSpawn, Payload: json.RawMessage(`{"prompt":"Say PEAR.","model":"haiku"}`)},
		`{"prompt":"Say PEAR."}`:                          protocol.Spawn{Prompt: "Say PEAR."},
		`{"prompt":"Review.","agent":"reviewer"}`:         protocol.Spawn{Prompt: "Review.", Agent: "reviewer"},
		`{"prompt":"Train.","requires":{"gpu":"nvidia"}}`: protocol.Spawn{Prompt: "Train.", Requires: map[string]string{"gpu": "nvidia"}},
		`{"prompt":"Anywhere.","requires":{}}`:            protocol.Spawn{Prompt: "Anywhere.", Requires: map[string]string{}},
		`{"task_id":"child-1"}`:                           protocol.Spawned{TaskID: "child-1"},
		`{"task_id":"child-1","tools":[]}`:                protocol.Spawned{TaskID: "child-1", Tools: []string{}},
		`{"to":"parent-1","text":"PEAR"}`:                 protocol.Send{To: "parent-1", Text: "PEAR"},
		`{"delivered":false}`:                             protocol.Sent{},
	} {
		got, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
	}
}
