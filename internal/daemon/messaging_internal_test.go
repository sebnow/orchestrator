package daemon

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// callTool calls a gateway tool as the agent of proc and returns the
// reply, failing the test if the tool reports an error.
func callTool(t *testing.T, proc *fakeProcess, tool string, arguments map[string]any) string {
	t.Helper()
	result, err := mustConnect(t, proc.spec.Gateway.URL).CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if result.IsError {
		t.Fatalf("%s: %s", tool, resultText(t, result))
	}
	return resultText(t, result)
}

type taskListing struct {
	ID       protocol.TaskID  `json:"id"`
	State    string           `json:"state"`
	ParentID *protocol.TaskID `json:"parent_id"`
}

func (f *serverFixture) tasks(t *testing.T) []taskListing {
	t.Helper()
	var tasks []taskListing
	f.call(t, http.MethodGet, "/v1/tasks", nil, &tasks)
	return tasks
}

func (f *serverFixture) waitForState(t *testing.T, task protocol.TaskID, state string) {
	t.Helper()
	eventually(t, string(task)+" to be "+state, func() bool {
		var detail taskListing
		f.call(t, http.MethodGet, "/v1/tasks/"+string(task), nil, &detail)
		return detail.State == state
	})
}

func TestGivenParentWhoseAgentSpawnsAChildWhenTheChildSendsToTheFinishedParentThenTheParentResumesWithAPromptFromTheChild(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	parent := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Spawn a child, then end your turn.", PauseLimits: testPauseLimits})
	parentProc := d.nextProcess(t)
	first := parentProc.nextInput(t)
	if tools := parentProc.spec.Gateway.Tools; !slices.Contains(tools, SpawnTaskTool) || !slices.Contains(tools, SendMessageTool) {
		t.Errorf("allowed gateway tools = %q", tools)
	}

	spawnReply := callTool(t, parentProc, SpawnTaskTool, map[string]any{"prompt": "Reply with PEAR and send it to your parent."})

	var child protocol.TaskID
	for _, task := range srv.tasks(t) {
		if task.ParentID != nil && *task.ParentID == parent {
			child = task.ID
		}
	}
	if child == "" || !strings.Contains(spawnReply, string(child)) {
		t.Fatalf("spawn reply %q; tasks %+v", spawnReply, srv.tasks(t))
	}
	childProc := d.nextProcess(t)
	if in := childProc.nextInput(t); in.text != "Reply with PEAR and send it to your parent." {
		t.Errorf("child's prompt = %q", in.text)
	}

	parentProc.emit(harness.Output{Line: []byte(`{"type":"result"}`), TurnEnded: true, Answering: []string{first.id}, SessionID: "parent-session"})
	expectExit(t, parentProc)
	srv.waitForState(t, parent, "finished")

	sendReply := callTool(t, childProc, SendMessageTool, map[string]any{"to": string(parent), "text": "PEAR"})

	if !strings.Contains(sendReply, "next prompt now") {
		t.Errorf("send reply = %q, want it delivered at once", sendReply)
	}
	resumed := d.nextProcess(t)
	if resumed.spec.Resume != "parent-session" {
		t.Errorf("resumed spec = %+v, want the parent's session", resumed.spec)
	}
	if in := resumed.nextInput(t); in.text != "Message from task "+string(child)+": PEAR" {
		t.Errorf("parent's next prompt = %q", in.text)
	}
	var from []protocol.TaskID
	for _, command := range srv.streamedCommands(t) {
		if command.TaskID != parent || command.Kind != protocol.CommandPrompt {
			continue
		}
		var prompt protocol.Prompt
		if err := json.Unmarshal(command.Payload, &prompt); err != nil {
			t.Fatal(err)
		}
		if prompt.From != nil {
			from = append(from, *prompt.From)
		}
	}
	if len(from) == 0 || from[0] != child {
		t.Errorf("senders of the parent's prompts = %q, want %s", from, child)
	}
}
