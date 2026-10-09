package server

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// seniorAgent may spawn but not message, and sets every default.
var seniorAgent = Agent{
	Name: "senior", Description: "Owns the design.", SystemPrompt: "You are the senior engineer.", Model: "sonnet",
	Tools: []string{protocol.ToolSpawnTask}, PauseLimits: &protocol.PauseLimits{Acknowledge: 30 * time.Second, Cleanup: 2 * time.Minute},
	Priority: PriorityHigh, Filler: true, Requires: Labels{},
}

func createAgents(t *testing.T, store *Store, agents ...Agent) {
	t.Helper()
	for _, a := range agents {
		if err := store.createAgent(t.Context(), a); err != nil {
			t.Fatal(err)
		}
	}
}

func readTask(t *testing.T, store *Store, task protocol.TaskID) taskDetail {
	t.Helper()
	detail, err := store.task(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	return detail
}

func TestGivenAgentWhenTheOwnerStartsATaskAsItThenTheTaskTakesWhatTheRequestLeavesOutAndItsPromptListsTheAgents(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	createAgents(t, srv.store, seniorAgent, Agent{Name: "junior", Description: "Does\nwhat it is told.", Tools: []string{}, Priority: PriorityNormal, Requires: Labels{}})

	turn := postForTurn(t, srv.url+"/v1/tasks", `{"daemon_id":"laptop","agent":"senior","prompt":"Plan.","system_prompt":"Be brief."}`, http.StatusCreated)

	detail := readTask(t, srv.store, turn.TaskID)
	if detail.Agent != "senior" || detail.Model != "sonnet" || detail.Priority != PriorityHigh || !detail.Filler ||
		detail.Start.PauseLimits != *seniorAgent.PauseLimits || !reflect.DeepEqual(detail.Start.Tools, []string{protocol.ToolSpawnTask}) {
		t.Errorf("task = %+v, start %+v; want the senior agent's defaults", detail.taskSummary, detail.Start)
	}
	wantPrompt := systemPrompt(nil, []string{protocol.ToolSpawnTask},
		"You can start a child task as one of these agents by giving its name as spawn_task's agent:\n- junior: Does what it is told.\n- senior: Owns the design.",
		"You are the senior engineer.", "Be brief.")
	if detail.Start.SystemPrompt != wantPrompt {
		t.Errorf("system prompt = %q\nwant %q", detail.Start.SystemPrompt, wantPrompt)
	}
}

func TestGivenAgentWhenTheRequestSetsItsOwnValuesThenTheyWin(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	createAgents(t, srv.store, seniorAgent)

	turn := postForTurn(t, srv.url+"/v1/tasks", `{"daemon_id":"laptop","agent":"senior","prompt":"Plan.","model":"opus","priority":"low","filler":false,`+
		`"tools":["send_message"],"pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`, http.StatusCreated)

	detail := readTask(t, srv.store, turn.TaskID)
	if detail.Model != "opus" || detail.Priority != PriorityLow || detail.Filler || detail.Start.PauseLimits != defaultPauseLimits ||
		!reflect.DeepEqual(detail.Start.Tools, []string{protocol.ToolSendMessage}) {
		t.Errorf("task = %+v, start %+v; want the request's values", detail.taskSummary, detail.Start)
	}
	if strings.Contains(detail.Start.SystemPrompt, "spawn_task") {
		t.Errorf("a task that may not spawn is told of spawn_task: %q", detail.Start.SystemPrompt)
	}
}

func TestGivenUnknownAgentWhenTheOwnerStartsATaskAsItThenUnprocessable(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/tasks", `{"daemon_id":"laptop","agent":"nobody","prompt":"p"}`)

	if status != http.StatusUnprocessableEntity || !strings.Contains(body, "unknown agent") {
		t.Errorf("status = %d (%s), want 422", status, body)
	}
}

func TestGivenNewTaskFormWithAnAgentAndBlankFieldsWhenSubmittedThenTheTaskTakesTheAgentsValuesAndShowsTheAgent(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	createAgents(t, srv.store, seniorAgent)
	page := getPage(t, srv.url+"/")
	requireContains(t, page, `<select name="agent"><option value="" selected="">None</option><option value="senior">senior</option></select>`)

	task := queueTaskViaForm(t, srv, "laptop", "Plan.", url.Values{"agent": {"senior"}, "acknowledge": {""}, "cleanup": {""}})

	detail := readTask(t, srv.store, task)
	if detail.Agent != "senior" || detail.Model != "sonnet" || detail.Priority != PriorityHigh || !detail.Filler || detail.Start.PauseLimits != *seniorAgent.PauseLimits {
		t.Errorf("task = %+v, start %+v; want the senior agent's values", detail.taskSummary, detail.Start)
	}
	requireContains(t, getPage(t, srv.url+"/tasks/"+string(task)), `<dt>Agent</dt><dd><a href="/agents/senior">senior</a></dd>`)
	requireContains(t, getPage(t, srv.url+"/"), `<td><a href="/agents/senior">senior</a></td>`)
}

func TestGivenNewTaskFormWithoutAnAgentAndBlankPauseLimitsWhenSubmittedThenTheTaskHasTheDefaults(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")

	task := queueTaskViaForm(t, srv, "laptop", "Plan.", url.Values{"acknowledge": {""}, "cleanup": {""}})

	detail := readTask(t, srv.store, task)
	if detail.Agent != "" || detail.Start.PauseLimits != defaultPauseLimits || detail.Priority != PriorityNormal || detail.Start.Tools != nil {
		t.Errorf("task = %+v, start %+v; want the defaults", detail.taskSummary, detail.Start)
	}
}
