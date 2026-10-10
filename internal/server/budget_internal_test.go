package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

var (
	accountA = budgetKey{Harness: "claude-code", Account: "a@example.com/org-1"}
	accountB = budgetKey{Harness: "claude-code", Account: "b@example.com/org-1"}
)

// rejectedFor is a rejected reading whose five-hour window resets in
// half an hour.
func rejectedFor() *quotaReading {
	return &quotaReading{QuotaObserved: protocol.QuotaObserved{Status: protocol.QuotaRejected, Windows: []protocol.QuotaWindow{
		{Name: fiveHourWindow, Utilization: 1, ResetsAt: schedNow.Add(30 * time.Minute)},
	}}, At: schedNow.Add(-time.Minute)}
}

func TestGivenTwoAccountsWhenOneIsRejectedThenOnlyTurnsGoingToItsDaemonsAreHeld(t *testing.T) {
	s := schedule{
		slots:    map[protocol.DaemonID]int{"laptop": 2, "vps": 1},
		keys:     map[protocol.DaemonID]budgetKey{"laptop": accountA, "vps": accountB},
		readings: map[budgetKey]*quotaReading{accountA: rejectedFor(), accountB: fiveHourAt(0.1)},
		turns: []pendingTurn{
			start(1, "bound", "laptop", PriorityNormal),
			placed(start(2, "anywhere", "laptop", PriorityNormal), placementAny),
			later(3, "running", turnPrompt, TaskRunning, "laptop"),
			filler(placed(start(4, "filler", "laptop", PriorityNormal), placementAny)),
		},
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{"anywhere@vps"})
	for _, turn := range []uint64{1, 3} {
		requireReason(t, d, turn, "rejected by the account's usage limits until 2026-10-08 12:30 UTC")
	}
	// The filler waits for the slot the normal turn took on vps, the one
	// daemon whose budget allows it.
	requireReason(t, d, 4, "slots")
	if want := schedNow.Add(30 * time.Minute); !d.wake.Equal(want) {
		t.Errorf("wake = %s, want the rejected window's reset %s", d.wake, want)
	}
}

func TestGivenDaemonsOnOneAccountWhenItIsRejectedThenAnUnboundTurnWaitsNamingTheRejection(t *testing.T) {
	s := schedule{
		slots:    map[protocol.DaemonID]int{"laptop": 2, "vps": 2},
		keys:     map[protocol.DaemonID]budgetKey{"laptop": accountA, "vps": accountA},
		readings: map[budgetKey]*quotaReading{accountA: rejectedFor()},
		turns:    []pendingTurn{placed(start(1, "anywhere", "laptop", PriorityNormal), placementAny)},
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{})
	if got := d.reasons[1]; got != "rejected by the account's usage limits until 2026-10-08 12:30 UTC" {
		t.Errorf("reason = %q", got)
	}
}

func TestGivenFillerWhenOnlyOneAccountHasSpareBudgetThenItGoesToThatAccountsDaemon(t *testing.T) {
	s := schedule{
		slots:    map[protocol.DaemonID]int{"laptop": 1, "vps": 4},
		keys:     map[protocol.DaemonID]budgetKey{"laptop": accountA, "vps": accountB},
		readings: map[budgetKey]*quotaReading{accountA: fiveHourAt(0.1), accountB: fiveHourAt(0.7)},
		turns:    []pendingTurn{filler(placed(start(1, "filler", "vps", PriorityNormal), placementAny))},
	}

	requireStrings(t, "admitted", admitted(decide(s, testPolicy, schedNow)), []string{"filler@laptop"})
}

func TestGivenDaemonsWithAndWithoutAnAccountWhenKeyedThenAnAccountIsSharedAndOtherwiseEachDaemonHasItsOwn(t *testing.T) {
	harness := &protocol.Harness{Name: "claude-code"}
	for name, tc := range map[string]struct {
		daemon  protocol.DaemonID
		facts   Labels
		harness *protocol.Harness
		want    budgetKey
	}{
		"account":              {"vps", Labels{"harness": "claude-code", "account": "a@example.com/org-1"}, nil, accountA},
		"no account":           {"vps", Labels{"harness": "claude-code"}, nil, budgetKey{Harness: "claude-code", Daemon: "vps"}},
		"harness from events":  {"vps", Labels{"account": "a@example.com/org-1"}, harness, accountA},
		"the fact wins":        {"vps", Labels{"harness": "claude-code", "account": "a@example.com/org-1"}, &protocol.Harness{Name: "other"}, accountA},
		"nothing reported yet": {"vps", Labels{}, nil, budgetKey{Daemon: "vps"}},
	} {
		if got := budgetKeyOf(tc.daemon, tc.facts, tc.harness); got != tc.want {
			t.Errorf("%s: key = %+v, want %+v", name, got, tc.want)
		}
	}
	if got := accountA.String(); got != "claude-code, account a@example.com/org-1" {
		t.Errorf("String = %q", got)
	}
}

// quotaFrom has task on daemon report the five-hour window used to
// utilization at at.
func quotaFrom(t *testing.T, store *Store, daemon protocol.DaemonID, task protocol.TaskID, seq uint64, at time.Time, utilization float64) {
	t.Helper()
	kind, payload := quotaEvent(utilization, at)
	e := event(task, seq, payload)
	e.Kind, e.Time = kind, at
	if _, _, err := store.appendEvents(t.Context(), daemon, []protocol.Event{e}); err != nil {
		t.Fatal(err)
	}
}

func TestGivenReadingsFromDaemonsOnTwoAccountsWhenStoredThenEachAccountKeepsItsOwnNewestReading(t *testing.T) {
	store, _ := openTestStore(t)
	for daemon, account := range map[protocol.DaemonID]string{"laptop": "a@example.com/org-1", "vps": "a@example.com/org-1", "gpu": "b@example.com/org-1"} {
		if err := store.setFacts(t.Context(), daemon, Labels{"harness": "claude-code", "account": account}); err != nil {
			t.Fatal(err)
		}
		seedTask(t, store, daemon, protocol.TaskID("task-"+daemon))
	}
	seedTask(t, store, "spare", "task-spare")
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	quotaFrom(t, store, "laptop", "task-laptop", 1, now.Add(-2*time.Minute), 0.2)
	quotaFrom(t, store, "vps", "task-vps", 1, now.Add(-time.Minute), 0.4)
	quotaFrom(t, store, "gpu", "task-gpu", 1, now, 0.9)
	quotaFrom(t, store, "spare", "task-spare", 1, now, 0.6)
	// The laptop logs in to account b; its earlier reading stays a's.
	if err := store.setFacts(t.Context(), "laptop", Labels{"harness": "claude-code", "account": "b@example.com/org-1"}); err != nil {
		t.Fatal(err)
	}

	readings, err := store.readings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	got := map[budgetKey]float64{}
	for _, reading := range readings {
		got[reading.key] = reading.Windows[0].Utilization
	}
	want := map[budgetKey]float64{accountA: 0.4, accountB: 0.9, {Harness: "claude-code", Daemon: "spare"}: 0.6}
	requireJSONEqual(t, len(got), len(want))
	for key, utilization := range want {
		if got[key] != utilization {
			t.Errorf("%s: utilization %v, want %v", key, got[key], utilization)
		}
	}
	for _, daemon := range daemonsByID(t, store) {
		if daemon.ID == "laptop" && (daemon.Quota == nil || daemon.Quota.Windows[0].Utilization != 0.9) {
			t.Errorf("laptop's quota = %+v, want account b's reading", daemon.Quota)
		}
	}
}

func daemonsByID(t *testing.T, store *Store) []daemonSummary {
	t.Helper()
	daemons, err := store.daemons(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return daemons
}

func TestGivenReadingStoredBeforeVersion23WhenMigratedThenItIsTheBudgetOfItsTasksDaemon(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.db")
	createVersionOneDatabase(t, path)
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), `INSERT INTO events (task_id, seq, kind, harness_name, harness_version, time, payload)
		VALUES ('old', 1, 'quota_observed', 'claude-code', '2.1.289', '2026-10-07T10:00:02Z',
			'{"status":"allowed","windows":[{"name":"five_hour","utilization":0.3,"resets_at":"2026-10-07T15:00:00Z"}]}')`)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	readings, err := store.readings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(readings) != 1 || readings[0].key != (budgetKey{Harness: "claude-code", Daemon: "laptop"}) {
		t.Errorf("readings = %+v, want the laptop's own", readings)
	}
}

func TestGivenReadingsOfTwoAccountsWhenTheDashboardIsShownThenEachBudgetIsNamed(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts", `{"harness":"claude-code","account":"a@example.com/org-1"}`)
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/vps/facts", `{"harness":"claude-code"}`)
	seedTask(t, srv.store, "laptop", "task-laptop")
	seedTask(t, srv.store, "vps", "task-vps")
	quotaFrom(t, srv.store, "laptop", "task-laptop", 1, time.Now(), 0.42)
	quotaFrom(t, srv.store, "vps", "task-vps", 1, time.Now(), 0.17)

	requireContains(t, getPage(t, srv.url+"/"),
		"<strong>claude-code, account a@example.com/org-1</strong>", "42% used",
		"<strong>claude-code, daemon vps</strong>", "17% used")
}
