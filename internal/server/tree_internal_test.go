package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// spawnFamily starts root, which spawns child-a and child-b, of which
// child-a spawns grandchild; child-a pushes a branch.
func spawnFamily(t *testing.T, store *Store) {
	t.Helper()
	taskIn(t, store, "root", TaskRunning)
	createAgents(t, store, Agent{Name: "reviewer", Tools: []string{}, Priority: PriorityNormal, Requires: Labels{}})
	for _, spawn := range []struct {
		parent, child protocol.TaskID
		spawn         protocol.Spawn
	}{
		{"root", "child-a", protocol.Spawn{Purpose: "Find the bug.", Prompt: "Look."}},
		{"root", "child-b", protocol.Spawn{Purpose: "Review the fix.", Prompt: "Review.", Agent: "reviewer"}},
	} {
		if _, err := store.spawnTask(t.Context(), "laptop", spawn.parent, spawn.child, spawn.spawn); err != nil {
			t.Fatal(err)
		}
	}
	admitTurns(t, store)
	childA := &lifecycle{t: t, store: store, task: "child-a"}
	childA.event(protocol.KindHarnessStarted, started, TaskRunning)
	childA.event(protocol.KindBranchPushed, `{"branch":"orchestrator/child-a","commit":"eb69b7b37fad09fd0170733cbb1f53dbc1502ae7","ahead":1,"uncommitted":0,"error":""}`, TaskRunning)
	if _, err := store.spawnTask(t.Context(), "laptop", "child-a", "grandchild", protocol.Spawn{Purpose: "Bisect.", Prompt: "Bisect it."}); err != nil {
		t.Fatal(err)
	}
}

func TestGivenTaskWithDescendantsWhenItsTreeIsRequestedThenItHasEachDescendantWithPurposeStateCostAndBranch(t *testing.T) {
	srv := startTestServer(t)
	spawnFamily(t, srv.store)

	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/tasks/root/tree", "")

	want := `{"id":"root","state":"running","cost_usd":0,"children":[` +
		`{"id":"child-a","state":"running","purpose":"Find the bug.","cost_usd":0,` +
		`"branch":{"branch":"orchestrator/child-a","commit":"eb69b7b37fad09fd0170733cbb1f53dbc1502ae7","ahead":1,"uncommitted":0,"error":""},` +
		`"children":[{"id":"grandchild","state":"queued","purpose":"Bisect.","cost_usd":0,"children":[]}]},` +
		`{"id":"child-b","state":"pending","agent":"reviewer","purpose":"Review the fix.","cost_usd":0,"children":[]}]}` + "\n"
	if status != http.StatusOK || body != want {
		t.Errorf("tree: %d\ngot  %s\nwant %s", status, body, want)
	}
	status, body = doRequest(t, http.MethodGet, srv.url+"/v1/tasks/child-a/tree", "")
	var sub taskNode
	if status != http.StatusOK || json.Unmarshal([]byte(body), &sub) != nil || sub.ID != "child-a" || len(sub.Children) != 1 || sub.Children[0].ID != "grandchild" {
		t.Errorf("subtree of child-a: %d %s", status, body)
	}
	if status, _ := doRequest(t, http.MethodGet, srv.url+"/v1/tasks/missing/tree", ""); status != http.StatusNotFound {
		t.Errorf("tree of an unknown task: %d, want 404", status)
	}
}

func TestGivenTaskWithDescendantsWhenItsPageIsShownThenItsTreeNestsThemAndAChildsPageShowsOnlyItsOwn(t *testing.T) {
	srv := startTestServer(t)
	spawnFamily(t, srv.store)

	requireContains(t, getPage(t, srv.url+"/tasks/root"),
		`<h2>Tree</h2><div id="task-tree"><ul class="tree"><li><span class="badge state-running">running</span> <a href="/tasks/root">count to three</a><span class="reason"> $0.0000</span><ul>`+
			`<li><span class="badge state-running">running</span> <a href="/tasks/child-a">Find the bug.</a><span class="reason"> $0.0000, <code>orchestrator/child-a</code> at <code>eb69b7b37fad</code></span><ul>`+
			`<li><span class="badge state-queued">queued</span> <a href="/tasks/grandchild">Bisect.</a><span class="reason"> $0.0000</span></li></ul></li>`+
			`<li><span class="badge state-pending">pending</span> <a href="/tasks/child-b">Review the fix.</a> as <a href="/agents/reviewer">reviewer</a><span class="reason"> $0.0000</span></li>`+
			`</ul></li></ul></div>`)
	child := getPage(t, srv.url+"/tasks/child-a")
	requireContains(t, child, `<div id="task-tree"><ul class="tree"><li><span class="badge state-running">running</span> <a href="/tasks/child-a">Find the bug.</a>`)
	requireLacks(t, child, `<a href="/tasks/child-b">`)
}
