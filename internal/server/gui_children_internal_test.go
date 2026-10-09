package server

import (
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenParentWithChildrenWhenItsPageIsShownThenItListsEachChildWithItsAgentStateAndLatestReport(t *testing.T) {
	srv := startTestServer(t)
	parent := taskIn(t, srv.store, "parent", TaskRunning)
	createAgents(t, srv.store, Agent{Name: "reviewer", Tools: []string{}, Priority: PriorityNormal, Requires: Labels{}})
	if _, err := srv.store.spawnTask(t.Context(), "laptop", "parent", "reviewed", protocol.Spawn{Prompt: "Review.", Agent: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	admitTurns(t, srv.store)
	reviewed := &lifecycle{t: t, store: srv.store, task: "reviewed"}
	spawned(t, srv.store, "parent", "silent")
	parent.event(protocol.KindHarnessExited, cleanly, TaskFinished)
	reviewed.event(protocol.KindHarnessStarted, started, TaskRunning)
	reviewed.event(protocol.KindHarnessOutput, agentSays(`Looks right.\nTwo nits.`), TaskRunning)
	reviewed.event(protocol.KindHarnessExited, cleanly, TaskFinished)

	page := getPage(t, srv.url+"/tasks/parent")

	requireContains(t, page,
		`<h2>Children</h2><div id="task-children"><table>`,
		`<tr><td><a href="/tasks/reviewed">reviewed</a></td><td><a href="/agents/reviewer">reviewer</a></td><td><span class="badge state-finished">finished</span></td><td>Looks right.…</td></tr>`,
		`<tr><td><a href="/tasks/silent">silent</a></td><td></td><td><span class="badge state-pending">pending</span></td><td><span class="empty">none yet</span></td></tr>`,
	)
	requireContains(t, getPage(t, srv.url+"/tasks/reviewed"), `<dt>Parent</dt><dd><a href="/tasks/parent">parent</a></dd>`)
	requireContains(t, getPage(t, srv.url+"/tasks/silent"), "It has not spawned any tasks.")
}
