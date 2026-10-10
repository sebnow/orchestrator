package server

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenSlotsFactsAndLabelsWhenReadingCapacityThenTheLabelCapsTheFactAndOneIsTheDefault(t *testing.T) {
	var policy SchedulePolicy
	for name, tc := range map[string]struct {
		facts, labels Labels
		want          int
	}{
		"fact alone":           {Labels{"slots": "3"}, nil, 3},
		"label caps the fact":  {Labels{"slots": "3"}, Labels{"slots": "1"}, 1},
		"label over the fact":  {Labels{"slots": "3"}, Labels{"slots": "8"}, 3},
		"label alone":          {nil, Labels{"slots": "2"}, 2},
		"zero fact":            {Labels{"slots": "0"}, nil, 0},
		"neither":              {Labels{"os": "linux"}, nil, 1},
		"malformed is ignored": {Labels{"slots": "many"}, Labels{"slots": "-1"}, 1},
	} {
		if got := policy.capacity(tc.facts, tc.labels); got != tc.want {
			t.Errorf("%s: capacity = %d, want %d", name, got, tc.want)
		}
	}
}

func TestGivenDaemonsReportingSlotsWhenTheSchedulerRunsThenEachRunsAsManyTasksAsItsCapacity(t *testing.T) {
	store, _ := openTestStore(t)
	for _, task := range []protocol.TaskID{"a", "b", "c"} {
		queueTask(t, store, ownersTask(task, "laptop"))
	}
	if err := store.setFacts(t.Context(), "laptop", Labels{"slots": "4"}); err != nil {
		t.Fatal(err)
	}
	if err := store.setLabels(t.Context(), "laptop", Labels{"slots": "2"}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.schedule(t.Context(), DefaultSchedulePolicy, time.Now(), []protocol.DaemonID{"laptop"}, time.Time{}); err != nil {
		t.Fatal(err)
	}

	admitted := 0
	for _, task := range []protocol.TaskID{"a", "b", "c"} {
		if readTask(t, store, task).Queue == nil {
			admitted++
		}
	}
	if admitted != 2 {
		t.Errorf("admitted %d tasks, want 2, the owner's cap on the daemon's 4 slots", admitted)
	}
}

func TestGivenDaemonWithSlotsWhenTheDashboardIsShownThenItSaysHowManyAreInUseOfItsCapacity(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts", `{"slots":"3"}`)

	requireContains(t, getPage(t, srv.url+"/"), "<td>0 of 3 in use</td>")

	refused := send(t, http.MethodPost, srv.url+"/daemons/laptop/labels", url.Values{"labels": {"slots=lots"}}, false)
	if refused.status != http.StatusUnprocessableEntity {
		t.Errorf("slots label that is not a count: status = %d, want 422", refused.status)
	}
	requireContains(t, refused.body, "slots must be a count of 0 or more")
}
