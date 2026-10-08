package server

import (
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenParentAndChildThatMessageWhenTheirPagesAndTheDashboardAreShownThenEachLinksTheOtherAndShowsTheMessages(t *testing.T) {
	srv := startTestServer(t)
	parent := taskIn(t, srv.store, "parent", TaskRunning)
	child := spawned(t, srv.store, "parent", "child")
	child.drive(TaskRunning)
	parent.event(protocol.KindHarnessExited, cleanly, TaskFinished)
	sendFrom(t, srv.store, "child", "parent", "PEAR")

	parentPage := getPage(t, srv.url+"/tasks/parent")
	childPage := getPage(t, srv.url+"/tasks/child")
	dashboard := getPage(t, srv.url+"/")

	requireContains(t, parentPage,
		`<dt>Children</dt><dd><a href="/tasks/child">child</a></dd>`,
		`Agent started child task <a href="/tasks/child">child</a>`,
		`Message from task <a href="/tasks/child">child</a></header><pre>`+"\n"+`PEAR</pre>`,
	)
	requireContains(t, childPage,
		`<dt>Parent</dt><dd><a href="/tasks/parent">parent</a></dd>`,
		`Agent sent a message to task <a href="/tasks/parent">parent</a></header><pre>`+"\n"+`PEAR</pre>`,
	)
	requireContains(t, dashboard, `<a href="/tasks/child">Say PEAR.</a><span class="reason"> ↳ child of <a href="/tasks/parent">parent</a></span>`)
}

func TestGivenWatchedParentWhenItsChildSpawnsReportsOrEndsThenTheParentsWatchersAreSignalled(t *testing.T) {
	srv := startTestServer(t)
	parent := taskIn(t, srv.store, "parent", TaskRunning)
	changed, stop := srv.WatchTask("parent")
	defer stop()

	child := spawned(t, srv.store, "parent", "child")
	if !signalled(changed) {
		t.Error("no signal after the child was spawned")
	}
	child.drive(TaskRunning)
	parent.event(protocol.KindHarnessExited, cleanly, TaskFinished)
	signalled(changed)
	sendFrom(t, srv.store, "child", "parent", "PEAR")
	if !signalled(changed) {
		t.Error("no signal after the child's message was delivered")
	}
	child.event(protocol.KindHarnessExited, nonZero, TaskFailed)
	if !signalled(changed) {
		t.Error("no signal after the child failed")
	}
}
