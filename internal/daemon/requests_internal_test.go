package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenServerReplyWhenForwardingAnAgentRequestThenTheReplyIsReturnedAndAFailureCarriesItsText(t *testing.T) {
	var gotPath, gotBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(data)
		if strings.Contains(gotBody, `"send"`) {
			http.Error(w, `refused: task "parent" has ended as stopped and takes no messages`, http.StatusUnprocessableEntity)
			return
		}
		w.Write([]byte(`{"task_id":"child-1"}`))
	}))
	defer server.Close()
	forward := forwardTo(server.Client(), mustParseURL(t, server.URL), "laptop")

	reply, err := forward(t.Context(), "task-1", protocol.AgentRequest{Kind: protocol.AgentSpawn, Payload: json.RawMessage(`{"prompt":"Say PEAR."}`)})
	if err != nil || string(reply) != `{"task_id":"child-1"}` {
		t.Errorf("spawn: %s, %v", reply, err)
	}
	if gotPath != "/v1/daemons/laptop/tasks/task-1/requests" || gotBody != `{"kind":"spawn","payload":{"prompt":"Say PEAR."}}` {
		t.Errorf("request = %s %s", gotPath, gotBody)
	}
	_, err = forward(t.Context(), "task-1", protocol.AgentRequest{Kind: protocol.AgentSend, Payload: json.RawMessage(`{"to":"parent","text":"PEAR"}`)})
	if err == nil || !strings.Contains(err.Error(), `task "parent" has ended as stopped`) {
		t.Errorf("send: %v, want the server's reason", err)
	}
}

func TestGivenDaemonWithoutAServerWhenAnAgentSpawnsThenItIsToldTheDaemonIsNotConnected(t *testing.T) {
	d := New(t.TempDir(), newFakeHarness(), nil, nil)

	_, err := d.spawnTask("task-1")(context.Background(), spawnTaskInput{Purpose: "Check a fruit.", Prompt: "Say PEAR."})

	if !errors.Is(err, errNotConnected) {
		t.Errorf("err = %v, want errNotConnected", err)
	}
}

func TestGivenSpawnedChildWhenTheSpawnerIsToldThenSendMessageIsNamedOnlyIfTheChildMayCallIt(t *testing.T) {
	cases := map[string]struct {
		reply    string
		named    bool
		handBack bool
	}{
		"every tool":      {`{"task_id":"child-1"}`, true, false},
		"send_message":    {`{"task_id":"child-1","tools":["send_message"]}`, true, false},
		"no tools":        {`{"task_id":"child-1","tools":[]}`, false, true},
		"only spawn_task": {`{"task_id":"child-1","tools":["spawn_task"]}`, false, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			d := New(t.TempDir(), newFakeHarness(), nil, nil)
			var sent protocol.Spawn
			d.forward = func(_ context.Context, _ protocol.TaskID, request protocol.AgentRequest) (json.RawMessage, error) {
				if err := json.Unmarshal(request.Payload, &sent); err != nil {
					return nil, err
				}
				return json.RawMessage(tc.reply), nil
			}

			text, err := d.spawnTask("task-1")(t.Context(), spawnTaskInput{Purpose: "Check a fruit.", Prompt: "Say PEAR."})

			if err != nil {
				t.Fatal(err)
			}
			if sent.Purpose != "Check a fruit." || sent.Prompt != "Say PEAR." {
				t.Errorf("spawn sent = %+v, want the purpose and prompt", sent)
			}
			if !strings.HasPrefix(text, "Started child task child-1.") {
				t.Errorf("text = %q, want it to name the child", text)
			}
			if got := strings.Contains(text, protocol.ToolSendMessage); got != tc.named {
				t.Errorf("text = %q, names send_message: %t, want %t", text, got, tc.named)
			}
			if got := strings.Contains(text, "final report is delivered to you"); got != tc.handBack {
				t.Errorf("text = %q, tells of the hand-back: %t, want %t", text, got, tc.handBack)
			}
		})
	}
}
