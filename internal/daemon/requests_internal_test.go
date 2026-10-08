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

	_, err := d.spawnTask("task-1")(context.Background(), spawnTaskInput{Prompt: "Say PEAR."})

	if !errors.Is(err, errNotConnected) {
		t.Errorf("err = %v, want errNotConnected", err)
	}
}
