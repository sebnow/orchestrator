package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const (
	pushedCommit = "0123456789abcdef0123456789abcdef01234567"
	laterCommit  = "fedcba9876543210fedcba9876543210fedcba98"
)

func TestGivenBranchPushedWhenTheTaskIsShownThenItsPageAndTheDashboardNameTheBranchAndTheCommit(t *testing.T) {
	srv := startTestServer(t)
	task := startTaskViaForm(t, srv, "laptop", "Write the parser")
	branch := "orchestrator/" + string(task)
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindBranchPushed, `{"branch":"`+branch+`","commit":"`+pushedCommit+`","ahead":2,"uncommitted":3,"error":""}`)
	events.ingest(t, srv, "laptop")

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page,
		`<dt>Branch</dt><dd><code>`+branch+`</code> at <code>0123456789ab</code>, 2 commits beyond the start; 3 files left uncommitted in the workspace</dd>`,
		"Pushed branch "+branch+" at 0123456789ab",
	)
	requireLacks(t, page, "push failed")
	requireContains(t, getPage(t, srv.url+"/"), `<td><code>`+branch+`</code></td>`)
	status, body := doRequest(t, http.MethodGet, srv.url+"/v1/tasks/"+string(task), "")
	if want := `"branch":{"branch":"` + branch + `","commit":"` + pushedCommit + `","ahead":2,"uncommitted":3,"error":""}`; status != http.StatusOK || !strings.Contains(body, want) {
		t.Errorf("GET task: %d %s; want it to hold %s", status, body, want)
	}
}

func TestGivenAFailedPushAfterASuccessfulOneWhenTheTaskIsShownThenTheFailureIsShown(t *testing.T) {
	srv := startTestServer(t)
	task := startTaskViaForm(t, srv, "laptop", "Write the parser")
	branch := "orchestrator/" + string(task)
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindBranchPushed, `{"branch":"`+branch+`","commit":"`+pushedCommit+`","ahead":1,"uncommitted":0,"error":""}`)
	events.add(protocol.KindBranchPushed, `{"branch":"`+branch+`","commit":"`+laterCommit+`","ahead":2,"uncommitted":0,"error":"git push: exit status 1: rejected (fetch first)"}`)
	events.ingest(t, srv, "laptop")

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page,
		`<code>`+branch+`</code>, push failed at <code>fedcba987654</code>, 2 commits beyond the start<p class="problem">git push: exit status 1: rejected (fetch first)</p>`,
		"Could not push branch "+branch+" at fedcba987654",
		"Pushed branch "+branch+" at 0123456789ab",
	)
	requireContains(t, getPage(t, srv.url+"/"), `<td><code>`+branch+`</code> <span class="badge state-failed">push failed</span></td>`)
}

func TestGivenBranchReportedWithOnlyUncommittedFilesWhenTheTaskIsShownThenThePageSaysSoAndNothingWasPushed(t *testing.T) {
	srv := startTestServer(t)
	task := startTaskViaForm(t, srv, "laptop", "Write the parser")
	branch := "orchestrator/" + string(task)
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindBranchPushed, `{"branch":"`+branch+`","commit":"`+pushedCommit+`","ahead":0,"uncommitted":2,"error":""}`)
	events.ingest(t, srv, "laptop")

	page := getPage(t, srv.url+"/tasks/"+string(task))

	requireContains(t, page,
		`<dt>Branch</dt><dd><code>`+branch+`</code> at <code>0123456789ab</code>, 0 commits beyond the start; 2 files left uncommitted in the workspace</dd>`,
		"Nothing to push on branch "+branch+" at 0123456789ab",
	)
	requireLacks(t, page, "Pushed branch")
}
