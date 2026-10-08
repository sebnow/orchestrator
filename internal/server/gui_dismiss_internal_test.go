package server

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const dismissButton = `<button type="submit" class="plain">Dismiss</button>`

// failedTask starts a task whose harness exits with an error.
func failedTask(t *testing.T, srv testServer, prompt string) protocol.TaskID {
	t.Helper()
	task := startTaskViaForm(t, srv, "laptop", prompt)
	events := &taskEvents{task: task}
	events.add(protocol.KindHarnessStarted, `{"pid":7,"model":"haiku","workdir":"/w"}`)
	events.add(protocol.KindHarnessExited, `{"exit_code":1}`)
	events.ingest(t, srv, "laptop")
	return task
}

func dismissAction(task protocol.TaskID) string {
	return `action="/tasks/` + string(task) + `/dismiss"`
}

func TestGivenFailedTaskWhenDismissedFromTheAttentionListThenTheDashboardHidesItUntilAskedToShowIt(t *testing.T) {
	srv := startTestServer(t)
	task := failedTask(t, srv, "Doomed work")
	before := getPage(t, srv.url+"/")
	requireContains(t, before, `<span class="reason">harness exited with code 1</span><div class="dismiss">`, dismissAction(task), dismissButton)

	got := send(t, http.MethodPost, srv.url+"/tasks/"+string(task)+"/dismiss", url.Values{"back": {"/"}}, false)

	if got.status != http.StatusSeeOther || got.header.Get("Location") != "/" {
		t.Fatalf("dismiss: %d to %q, want 303 to /", got.status, got.header.Get("Location"))
	}
	after := getPage(t, srv.url+"/")
	requireContains(t, after, "Nothing needs attention.", "No tasks yet.", "1 dismissed task hidden. ", `<a href="/?dismissed=show">Show them</a>`)
	requireLacks(t, after, "Doomed work")
	shown := getPage(t, srv.url+"/?dismissed=show")
	requireContains(t, shown, "Nothing needs attention.", `<a href="/tasks/`+string(task)+`">Doomed work</a>`,
		`<span class="reason"> dismissed</span>`, "Showing 1 dismissed task. ", `hx-get="/?dismissed=show"`)
}

func TestGivenStoppedTaskWhenDismissedFromItsPageThenThePageSaysSoAndOffersNoDismissal(t *testing.T) {
	srv := startTestServer(t)
	task := taskThatExited(t, srv, false)
	postForm(t, srv, task, url.Values{"kind": {"stop"}})
	before := getPage(t, srv.url+"/tasks/"+string(task))
	requireContains(t, before, dismissAction(task), `<input name="back" type="hidden" value="/tasks/`+string(task)+`">`)

	got := send(t, http.MethodPost, srv.url+"/tasks/"+string(task)+"/dismiss", url.Values{"back": {"/tasks/" + string(task)}}, true)

	if got.status != http.StatusOK {
		t.Fatalf("dismiss: %d %s", got.status, got.body)
	}
	requireContains(t, got.body, `hx-swap-oob="innerHTML:#task-header"`, "<dt>Dismissed</dt>")
	requireLacks(t, got.body, dismissButton)
	requireContains(t, getPage(t, srv.url+"/tasks/"+string(task)), "<dt>Dismissed</dt>")
}

func TestGivenDismissFromTheDashboardWithHTMXWhenPostedThenTheDashboardIsSwappedInPlace(t *testing.T) {
	srv := startTestServer(t)
	task := failedTask(t, srv, "Doomed work")

	got := send(t, http.MethodPost, srv.url+"/tasks/"+string(task)+"/dismiss", url.Values{"back": {"/"}}, true)

	if got.status != http.StatusOK {
		t.Fatalf("dismiss: %d %s", got.status, got.body)
	}
	requireContains(t, got.body, `hx-swap-oob="innerHTML:#dashboard"`, "Nothing needs attention.", "1 dismissed task hidden.")
}

func TestGivenDismissWithAForeignReturnAddressWhenPostedThenTheOwnerReturnsToTheTask(t *testing.T) {
	srv := startTestServer(t)
	task := failedTask(t, srv, "Doomed work")

	got := send(t, http.MethodPost, srv.url+"/tasks/"+string(task)+"/dismiss", url.Values{"back": {"https://example.com/"}}, false)

	if location := got.header.Get("Location"); got.status != http.StatusSeeOther || location != "/tasks/"+string(task) {
		t.Errorf("dismiss: %d to %q, want 303 to the task", got.status, location)
	}
}

func TestGivenRunningTaskWhenItsPageIsShownOrItIsDismissedThenItOffersNoDismissalAndTheDismissalConflicts(t *testing.T) {
	srv := startTestServer(t)
	task := startTaskViaForm(t, srv, "laptop", "Live work")

	page := getPage(t, srv.url+"/tasks/"+string(task))
	got := send(t, http.MethodPost, srv.url+"/tasks/"+string(task)+"/dismiss", url.Values{"back": {"/"}}, false)

	requireLacks(t, page, dismissButton)
	if got.status != http.StatusConflict {
		t.Errorf("dismiss: %d %s, want 409", got.status, got.body)
	}
}
