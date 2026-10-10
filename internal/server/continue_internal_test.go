package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// finishedResult is a result whose usage says the session's context is
// 18 + 37,544 + 7,518 = 45,080 tokens of a 200,000-token window.
const finishedResult = `{"type":"result","subtype":"success","is_error":false,"result":"Done: the fix is on the branch.","total_cost_usd":0.02,` +
	`"usage":{"input_tokens":18,"output_tokens":329,"cache_creation_input_tokens":7518,"cache_read_input_tokens":37544},` +
	`"modelUsage":{"claude-haiku-4-5":{"contextWindow":200000}}}`

// taskThatReplied starts a task, through the API with body, whose first
// turn ends with the agent writing reply and finishedResult.
func taskThatReplied(t *testing.T, srv testServer, body, reply string) protocol.TaskID {
	t.Helper()
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	turn := postForTurn(t, srv.url+"/v1/tasks", body, http.StatusCreated)
	admitTurns(t, srv.store)
	events := &taskEvents{task: turn.TaskID}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindHarnessOutput, `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":`+jsonString(reply)+`}]}}`)
	events.add(protocol.KindHarnessOutput, finishedResult)
	events.add(protocol.KindHarnessExited, `{"exit_code":0}`)
	events.ingest(t, srv, "laptop")
	return turn.TaskID
}

func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

const plainTaskBody = `{"daemon_id":"laptop","prompt":"Fix it.","pause_limits":{"acknowledge":"1m","cleanup":"5m"}}`

func TestGivenFinishedTaskWhenItsPageIsShownThenItGivesTheContextSizeAndWhatResumingReads(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatReplied(t, srv, plainTaskBody, "Done: the fix is on the branch.")

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page, "<dt>Context</dt><dd>45,080 of 200,000 tokens</dd>",
		"Resuming reads about 45,080 tokens of context.", "caches the conversation for an hour",
		"https://code.claude.com/docs/en/prompt-caching#resuming-a-session", "Continue in a new task")
}

func TestGivenRunningTaskWhenItsPageIsShownThenItOffersNoContinuationNorResumeNote(t *testing.T) {
	srv := startTestServer(t)
	task, _ := startedTurn(t, srv)

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireLacks(t, page, "Continue in a new task", "Resuming reads")
}

func TestGivenFinishedTaskWhenContinuedInANewTaskThenTheNewTaskStartsFromItsFinalReplyAsItsSuccessor(t *testing.T) {
	srv := startTestServer(t)
	// Filler waits for a quota reading, so the agent runs as normal work.
	senior := seniorAgent
	senior.Filler = false
	createAgents(t, srv.store, senior)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	advertise(t, srv.store, "laptop", "opus;sonnet")
	project := createProject(t, srv.store, Project{Name: "tools", Repo: "https://example.com/tools.git", Ref: "main"})
	old := taskThatReplied(t, srv, `{"daemon_id":"laptop","agent":"senior","project":"`+project.ID+`","purpose":"Fix the parser.","prompt":"Fix it."}`,
		"Done: the fix is on the branch.")

	turn := postForTurn(t, srv.url+"/v1/tasks/"+string(old)+"/continue", "", http.StatusCreated)

	next := readTask(t, srv.store, turn.TaskID)
	if next.Continues == nil || *next.Continues != old || next.ParentID != nil {
		t.Errorf("continues = %v, parent %v; want %s and no parent", next.Continues, next.ParentID, old)
	}
	if next.Agent != "senior" || next.Project != project.ID || next.Purpose != "Fix the parser." || next.Model != "sonnet" || next.Priority != PriorityHigh {
		t.Errorf("new task = %+v, want the old one's agent, project, purpose, model and priority", next.taskSummary)
	}
	want := "Purpose: Fix the parser.\n\nDone: the fix is on the branch.\n\n(Orchestrator: No branch pushed for task " + string(old) + ".)"
	if next.Start.Prompt != want {
		t.Errorf("prompt = %q, want %q", next.Start.Prompt, want)
	}
	if prior := readTask(t, srv.store, old); len(prior.ContinuedBy) != 1 || prior.ContinuedBy[0] != turn.TaskID {
		t.Errorf("continued by = %v, want %s", prior.ContinuedBy, turn.TaskID)
	}
	requireContains(t, getPage(t, srv.url+"/tasks/"+string(turn.TaskID)), `<dt>Continues</dt><dd><a href="/tasks/`+string(old)+`">`)
	requireContains(t, getPage(t, srv.url+"/tasks/"+string(old)), `<dt>Continued by</dt><dd><a href="/tasks/`+string(turn.TaskID)+`">`)
}

func TestGivenFinishedTaskWhenTheContinueFormIsPostedThenTheBrowserIsSentToTheNewTask(t *testing.T) {
	srv := startTestServer(t)
	old := taskThatReplied(t, srv, plainTaskBody, "Halfway there.")

	got := send(t, http.MethodPost, srv.url+"/tasks/"+string(old)+"/continue", url.Values{}, false)

	next, found := strings.CutPrefix(got.header.Get("Location"), "/tasks/")
	if got.status != http.StatusSeeOther || !found || next == string(old) {
		t.Fatalf("status %d, location %q; want the new task's page", got.status, got.header.Get("Location"))
	}
	if detail := readTask(t, srv.store, protocol.TaskID(next)); !strings.HasPrefix(detail.Start.Prompt, "Halfway there.") {
		t.Errorf("prompt = %q", detail.Start.Prompt)
	}
}

func TestGivenRunningTaskWhenContinuedThenConflict(t *testing.T) {
	srv := startTestServer(t)
	task, _ := startedTurn(t, srv)

	status, body := doRequest(t, http.MethodPost, srv.url+"/v1/tasks/"+string(task)+"/continue", "")

	if status != http.StatusConflict {
		t.Errorf("status = %d (%s), want 409", status, body)
	}
}
