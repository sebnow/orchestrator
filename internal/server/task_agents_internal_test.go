package server

import (
	"errors"
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
	Name: "senior", Description: "Owns the design.", SystemPrompt: "You are the senior engineer.", Models: []string{"sonnet"},
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
	if detail.Agent != "senior" || detail.Model != "" || !reflect.DeepEqual(detail.Models, []string{"sonnet"}) || detail.Priority != PriorityHigh || !detail.Filler ||
		detail.Start.PauseLimits != *seniorAgent.PauseLimits || !reflect.DeepEqual(detail.Start.Tools, []string{protocol.ToolSpawnTask}) {
		t.Errorf("task = %+v, start %+v; want the senior agent's defaults", detail.taskSummary, detail.Start)
	}
	wantPrompt := toolsPrompt([]string{protocol.ToolSpawnTask}) + "\n\n" +
		"You can start a child task as one of these agents by giving its name as spawn_task's agent:\n- junior: Does what it is told.\n- senior: Owns the design.\n\n" +
		"You are the senior engineer.\n\nBe brief."
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
	if detail.Model != "opus" || detail.Models != nil || detail.Priority != PriorityLow || detail.Filler || detail.Start.PauseLimits != defaultPauseLimits ||
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
	if detail.Agent != "senior" || !reflect.DeepEqual(detail.Models, []string{"sonnet"}) || detail.Priority != PriorityHigh || !detail.Filler || detail.Start.PauseLimits != *seniorAgent.PauseLimits {
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

func TestGivenRunningParentWhenItSpawnsAChildAsAnAgentThenTheChildHasTheAgentsSettingsAndTheParentsWhereTheAgentSetsNone(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "parent", TaskRunning)
	createAgents(t, store, Agent{Name: "reviewer", SystemPrompt: "You review.", Models: []string{"opus"}, Tools: []string{}, Priority: PriorityLow, Requires: Labels{}})

	if _, err := store.spawnTask(t.Context(), "laptop", "parent", "child", protocol.Spawn{Prompt: "Review.", Agent: "reviewer"}); err != nil {
		t.Fatal(err)
	}

	child, parent := readTask(t, store, "child"), readTask(t, store, "parent")
	if child.Agent != "reviewer" || child.Model != "" || !reflect.DeepEqual(child.Models, []string{"opus"}) || child.Priority != PriorityLow || child.Start.PauseLimits != parent.Start.PauseLimits ||
		child.Start.Tools == nil || len(child.Start.Tools) != 0 {
		t.Errorf("child = %+v, start %+v; want the reviewer's settings and the parent's pause limits", child.taskSummary, child.Start)
	}
	want := systemPrompt(promptParts{Parent: fromTask("parent"), Tools: []string{}, Agent: "You review."})
	if child.Start.SystemPrompt != want {
		t.Errorf("system prompt = %q\nwant %q", child.Start.SystemPrompt, want)
	}
}

func TestGivenSpawnNamingNoAgentThereIsWhenRequestedThenItIsRefusedForTheAgentToRead(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "parent", TaskRunning)

	_, err := store.spawnTask(t.Context(), "laptop", "parent", "child", protocol.Spawn{Prompt: "Review.", Agent: "nobody"})

	if !errors.Is(err, errRefused) || !strings.Contains(err.Error(), `there is no agent "nobody"`) {
		t.Errorf("err = %v, want a refusal naming the agent", err)
	}
}

func TestGivenTaskNotAllowedAToolWhenItsAgentCallsItAnywayThenTheServerRefuses(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "task", TaskRunning)
	taskIn(t, store, "other", TaskRunning)
	if _, err := store.db.ExecContext(t.Context(), `UPDATE tasks SET tools = '[]' WHERE id = 'task'`); err != nil {
		t.Fatal(err)
	}

	_, spawnErr := store.spawnTask(t.Context(), "laptop", "task", "child", protocol.Spawn{Prompt: "p"})
	_, sendErr := store.sendMessage(t.Context(), "laptop", "task", protocol.Send{To: "other", Text: "hi"})

	if !errors.Is(spawnErr, errRefused) || !errors.Is(sendErr, errRefused) {
		t.Errorf("spawn err = %v, send err = %v; want both refused", spawnErr, sendErr)
	}
}

func TestGivenParentAllowedOnlySpawnTaskWhenItSpawnsAChildAsNoAgentThenTheChildInheritsItsTools(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "parent", TaskRunning)
	if _, err := store.db.ExecContext(t.Context(), `UPDATE tasks SET tools = '["spawn_task"]' WHERE id = 'parent'`); err != nil {
		t.Fatal(err)
	}

	if _, err := store.spawnTask(t.Context(), "laptop", "parent", "child", protocol.Spawn{Prompt: "Work."}); err != nil {
		t.Fatal(err)
	}

	child := readTask(t, store, "child")
	if !reflect.DeepEqual(child.Start.Tools, []string{protocol.ToolSpawnTask}) {
		t.Errorf("child tools = %#v, want the parent's [spawn_task]", child.Start.Tools)
	}
	if strings.Contains(child.Start.SystemPrompt, "send_message") {
		t.Errorf("a child that may not message is told of send_message: %q", child.Start.SystemPrompt)
	}
}

func TestGivenParentAllowedEveryToolWhenItSpawnsAChildAsNoAgentThenTheChildIsAllowedEveryTool(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "parent", TaskRunning)

	if _, err := store.spawnTask(t.Context(), "laptop", "parent", "child", protocol.Spawn{Prompt: "Work."}); err != nil {
		t.Fatal(err)
	}

	if tools := readTask(t, store, "child").Start.Tools; tools != nil {
		t.Errorf("child tools = %#v, want nil, every tool, as its parent", tools)
	}
}

func TestGivenParentAllowedOnlySpawnTaskWhenItSpawnsAChildAsAnAgentThenTheChildHasOnlyTheAgentsToolsTheParentHas(t *testing.T) {
	for name, tc := range map[string]struct {
		agentTools []string
		want       []string
	}{
		"agent allows both":           {agentTools: []string{protocol.ToolSendMessage, protocol.ToolSpawnTask}, want: []string{protocol.ToolSpawnTask}},
		"agent allows only the other": {agentTools: []string{protocol.ToolSendMessage}, want: []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			store, _ := openTestStore(t)
			taskIn(t, store, "parent", TaskRunning)
			if _, err := store.db.ExecContext(t.Context(), `UPDATE tasks SET tools = '["spawn_task"]' WHERE id = 'parent'`); err != nil {
				t.Fatal(err)
			}
			createAgents(t, store, Agent{Name: "worker", Tools: tc.agentTools, Priority: PriorityNormal, Requires: Labels{}})

			if _, err := store.spawnTask(t.Context(), "laptop", "parent", "child", protocol.Spawn{Prompt: "Work.", Agent: "worker"}); err != nil {
				t.Fatal(err)
			}

			if tools := readTask(t, store, "child").Start.Tools; !reflect.DeepEqual(tools, tc.want) {
				t.Errorf("child tools = %#v, want %#v", tools, tc.want)
			}
		})
	}
}

func TestGivenAgentWithAnEffortWhenTasksAreStartedAsItThenTheyTakeItUnlessTheyNameTheirOwn(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	createAgents(t, srv.store, Agent{Name: "thinker", Effort: protocol.EffortMax, Tools: []string{}, Priority: PriorityNormal, Requires: Labels{}})

	inherited := postForTurn(t, srv.url+"/v1/tasks", `{"daemon_id":"laptop","agent":"thinker","prompt":"Think."}`, http.StatusCreated)
	own := postForTurn(t, srv.url+"/v1/tasks", `{"daemon_id":"laptop","agent":"thinker","prompt":"Think.","effort":"low"}`, http.StatusCreated)
	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/tasks", `{"daemon_id":"laptop","prompt":"Think.","effort":"xhigh","pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`)

	if got := readTask(t, srv.store, inherited.TaskID).Start.Effort; got != protocol.EffortMax {
		t.Errorf("effort of a task naming none = %q, want the agent's max", got)
	}
	if got := readTask(t, srv.store, own.TaskID).Start.Effort; got != protocol.EffortLow {
		t.Errorf("effort of a task naming low = %q, want low", got)
	}
	if status != http.StatusBadRequest || !strings.Contains(body, `effort "xhigh"`) {
		t.Errorf("unknown effort: %d %s, want 400", status, body)
	}
}

func TestGivenParentWithAnEffortWhenItSpawnsThenAChildTakesItsAgentsEffortOrElseTheParents(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "parent", TaskRunning)
	if _, err := store.db.ExecContext(t.Context(), `UPDATE tasks SET effort = 'high' WHERE id = 'parent'`); err != nil {
		t.Fatal(err)
	}
	createAgents(t, store,
		Agent{Name: "quick", Effort: protocol.EffortLow, Tools: []string{}, Priority: PriorityNormal, Requires: Labels{}},
		Agent{Name: "plain", Tools: []string{}, Priority: PriorityNormal, Requires: Labels{}})

	for child, agent := range map[protocol.TaskID]string{"quick-child": "quick", "plain-child": "plain", "bare-child": ""} {
		if _, err := store.spawnTask(t.Context(), "laptop", "parent", child, protocol.Spawn{Prompt: "Work.", Agent: agent}); err != nil {
			t.Fatal(err)
		}
	}

	for child, want := range map[protocol.TaskID]string{"quick-child": "low", "plain-child": "high", "bare-child": "high"} {
		if got := readTask(t, store, child).Start.Effort; got != want {
			t.Errorf("%s effort = %q, want %q", child, got, want)
		}
	}
}
