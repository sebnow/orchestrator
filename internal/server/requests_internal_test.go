package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// postAgentRequest makes an agent request for task as daemon.
func postAgentRequest(t *testing.T, srv testServer, daemon protocol.DaemonID, task protocol.TaskID, kind protocol.AgentRequestKind, payload any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(protocol.AgentRequest{Kind: kind, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	return doRequest(t, http.MethodPost, srv.url+"/v1/daemons/"+string(daemon)+"/tasks/"+string(task)+"/requests", string(body))
}

func TestGivenRunningTaskWhenItsAgentSpawnsThenTheReplyNamesANewChildOfTheTask(t *testing.T) {
	srv := startTestServer(t)
	taskIn(t, srv.store, "parent", TaskRunning)

	status, body := postAgentRequest(t, srv, "laptop", "parent", protocol.AgentSpawn, protocol.Spawn{Purpose: "Check a fruit.", Prompt: "Say PEAR."})

	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var spawned protocol.Spawned
	if err := json.Unmarshal([]byte(body), &spawned); err != nil {
		t.Fatal(err)
	}
	child, err := srv.store.task(t.Context(), spawned.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentID == nil || *child.ParentID != "parent" || child.Start.Prompt != "Purpose: Check a fruit.\n\nSay PEAR." {
		t.Errorf("child = %+v, start %+v", child.taskSummary, child.Start)
	}
}

func TestGivenParentInAProjectWhenItsAgentSpawnsWithAPurposeThenTheChildKeepsThePurposeAtTheTopOfItsPromptAndBelongsToTheProject(t *testing.T) {
	srv := startTestServer(t)
	taskIn(t, srv.store, "parent", TaskRunning)
	p := createProject(t, srv.store, Project{Name: "tools"})
	if _, err := srv.store.db.ExecContext(t.Context(), `UPDATE tasks SET project = ? WHERE id = 'parent'`, p.ID); err != nil {
		t.Fatal(err)
	}

	status, body := postAgentRequest(t, srv, "laptop", "parent", protocol.AgentSpawn,
		protocol.Spawn{Purpose: " Find the flaky test;\n report its name. ", Prompt: "Run the suite three times."})

	var spawned protocol.Spawned
	if status != http.StatusOK || json.Unmarshal([]byte(body), &spawned) != nil {
		t.Fatalf("status %d: %s", status, body)
	}
	child := readTask(t, srv.store, spawned.TaskID)
	if child.Purpose != "Find the flaky test; report its name." || child.Project != p.ID ||
		child.Start.Prompt != "Purpose: Find the flaky test; report its name.\n\nRun the suite three times." {
		t.Errorf("child = %+v, start %+v; want the purpose on one line, at the top of the prompt, and the parent's project", child.taskSummary, child.Start)
	}
	status, body = doRequest(t, http.MethodGet, srv.url+"/v1/tasks/"+string(spawned.TaskID), "")
	if status != http.StatusOK || !strings.Contains(body, `"purpose":"Find the flaky test; report its name."`) || !strings.Contains(body, `"project":"`+p.ID+`"`) ||
		!strings.Contains(body, `"parent_id":"parent"`) {
		t.Errorf("child JSON: %d %s; want its purpose, project and parent", status, body)
	}
}

func TestGivenSpawnWithoutAPurposeWhenRequestedThenTheAgentIsToldOneIsNeededAndNoChildStarts(t *testing.T) {
	srv := startTestServer(t)
	taskIn(t, srv.store, "parent", TaskRunning)

	for _, purpose := range []string{"", " \n "} {
		status, body := postAgentRequest(t, srv, "laptop", "parent", protocol.AgentSpawn, protocol.Spawn{Purpose: purpose, Prompt: "Say PEAR."})

		if status != http.StatusBadRequest || !strings.Contains(body, "the child task needs a purpose: one line saying why it exists and what you expect back") {
			t.Errorf("purpose %q: status %d %s, want 400 saying a purpose is needed", purpose, status, body)
		}
	}
	if children, err := srv.store.children(t.Context(), "parent"); err != nil || len(children) != 0 {
		t.Errorf("children = %+v, %v; want none", children, err)
	}
}

func TestGivenAgentWithoutAgentToolsWhenATaskSpawnsItThenTheReplyNamesNoTools(t *testing.T) {
	srv := startTestServer(t)
	taskIn(t, srv.store, "parent", TaskRunning)
	createAgents(t, srv.store, Agent{Name: "worker", Tools: []string{}, Priority: PriorityNormal, Requires: Labels{}})

	status, body := postAgentRequest(t, srv, "laptop", "parent", protocol.AgentSpawn, protocol.Spawn{Purpose: "Do the work.", Prompt: "Work.", Agent: "worker"})

	if status != http.StatusOK {
		t.Fatalf("status %d: %s", status, body)
	}
	var spawned protocol.Spawned
	if err := json.Unmarshal([]byte(body), &spawned); err != nil {
		t.Fatal(err)
	}
	if spawned.Tools == nil || len(spawned.Tools) != 0 {
		t.Errorf("tools = %#v in %s, want none", spawned.Tools, body)
	}
}

func TestGivenRunningTaskWhenItsAgentSendsToAFinishedTaskThenTheReplySaysItWasDelivered(t *testing.T) {
	srv := startTestServer(t)
	taskIn(t, srv.store, "sender", TaskRunning)
	taskIn(t, srv.store, "recipient", TaskFinished)

	status, body := postAgentRequest(t, srv, "laptop", "sender", protocol.AgentSend, protocol.Send{To: "recipient", Text: "PEAR"})

	if status != http.StatusOK || strings.TrimSpace(body) != `{"delivered":true}` {
		t.Errorf("status %d: %s", status, body)
	}
}

func TestGivenTaskOfAnotherDaemonWhenADaemonMakesARequestForItThenForbidden(t *testing.T) {
	srv := startTestServer(t)
	taskIn(t, srv.store, "agent", TaskRunning)
	if _, err := srv.store.heldSeqs(t.Context(), "vps"); err != nil {
		t.Fatal(err)
	}

	for _, task := range []protocol.TaskID{"agent", "nobody"} {
		spawn, spawnBody := postAgentRequest(t, srv, "vps", task, protocol.AgentSpawn, protocol.Spawn{Purpose: "Check a fruit.", Prompt: "Say PEAR."})
		send, sendBody := postAgentRequest(t, srv, "vps", task, protocol.AgentSend, protocol.Send{To: "agent", Text: "hi"})

		if spawn != http.StatusForbidden || send != http.StatusForbidden {
			t.Errorf("as %s: spawn %d %s; send %d %s; want 403", task, spawn, spawnBody, send, sendBody)
		}
	}
}

func TestGivenTaskWithoutARunningProcessWhenARequestIsMadeForItThenConflict(t *testing.T) {
	srv := startTestServer(t)
	taskIn(t, srv.store, "agent", TaskFinished)
	taskIn(t, srv.store, "recipient", TaskRunning)

	spawn, spawnBody := postAgentRequest(t, srv, "laptop", "agent", protocol.AgentSpawn, protocol.Spawn{Purpose: "Check a fruit.", Prompt: "Say PEAR."})
	send, sendBody := postAgentRequest(t, srv, "laptop", "agent", protocol.AgentSend, protocol.Send{To: "recipient", Text: "hi"})

	if spawn != http.StatusConflict || send != http.StatusConflict {
		t.Errorf("spawn %d %s; send %d %s; want 409", spawn, spawnBody, send, sendBody)
	}
}

func TestGivenEndedRecipientWhenAnAgentSendsToItThenTheReplySaysWhy(t *testing.T) {
	srv := startTestServer(t)
	taskIn(t, srv.store, "sender", TaskRunning)
	taskIn(t, srv.store, "recipient", TaskStopped)

	status, body := postAgentRequest(t, srv, "laptop", "sender", protocol.AgentSend, protocol.Send{To: "recipient", Text: "hi"})

	if status != http.StatusUnprocessableEntity || !strings.Contains(body, `task "recipient" has ended as stopped`) {
		t.Errorf("status %d: %s", status, body)
	}
}

func TestGivenMalformedAgentRequestWhenItArrivesThenBadRequest(t *testing.T) {
	srv := startTestServer(t)
	taskIn(t, srv.store, "agent", TaskRunning)
	for name, request := range map[string]struct {
		kind    protocol.AgentRequestKind
		payload any
	}{
		"unknown kind":         {"read_inbox", map[string]string{}},
		"spawn without prompt": {protocol.AgentSpawn, protocol.Spawn{Purpose: "Check a fruit.", Prompt: " "}},
		"spawn with a typo":    {protocol.AgentSpawn, map[string]string{"promt": "Say PEAR."}},
		"send without text":    {protocol.AgentSend, protocol.Send{To: "agent", Text: ""}},
		"send to a bad id":     {protocol.AgentSend, protocol.Send{To: "../x", Text: "hi"}},
	} {
		if status, body := postAgentRequest(t, srv, "laptop", "agent", request.kind, request.payload); status != http.StatusBadRequest {
			t.Errorf("%s: status %d %s, want 400", name, status, body)
		}
	}
}

func TestGivenOwnerPromptNamingASenderWhenPostedThenBadRequest(t *testing.T) {
	srv := startTestServer(t)
	taskIn(t, srv.store, "task-1", TaskFinished)

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/tasks/task-1/commands", `{"kind":"prompt","payload":{"text":"PEAR","from":"task-2"}}`)

	if status != http.StatusBadRequest {
		t.Errorf("status %d: %s", status, body)
	}
}
