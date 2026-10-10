package server

import (
	"errors"
	"maps"
	"net/http"
	"net/url"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func requiring(turn pendingTurn, requires Labels) pendingTurn {
	turn.Requires = requires
	return turn
}

// labelled are a laptop with no GPU and two free slots, and a vps with
// an NVIDIA GPU and one.
var labelled = schedule{
	slots:  map[protocol.DaemonID]int{"laptop": 2, "vps": 1},
	labels: map[protocol.DaemonID]Labels{"laptop": {"os": "darwin"}, "vps": {"os": "linux", "gpu": "nvidia"}},
}

func TestGivenTaskRequiringALabelWhenPlacedThenItGoesToADaemonWithItOverOneWithMoreFreeSlots(t *testing.T) {
	s := labelled
	s.turns = []pendingTurn{requiring(placed(start(1, "train", "laptop", PriorityNormal), placementAny), Labels{"gpu": "nvidia"})}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{"train@vps"})
}

func TestGivenTaskRequiringWhatNoConnectedDaemonHasWhenPlacedThenItWaitsNamingTheLabels(t *testing.T) {
	s := labelled
	s.turns = []pendingTurn{
		requiring(placed(start(1, "missing", "laptop", PriorityNormal), placementAny), Labels{"gpu": "amd", "os": "linux"}),
		requiring(placed(start(2, "split", "laptop", PriorityNormal), placementAny), Labels{"gpu": "nvidia", "os": "darwin"}),
		requiring(start(3, "bound", "laptop", PriorityNormal), Labels{"gpu": "nvidia"}),
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{})
	if got := d.reasons[1]; got != "no daemon has gpu=amd" {
		t.Errorf("reason = %q, want only the label no daemon has", got)
	}
	if got := d.reasons[2]; got != "no daemon has gpu=nvidia, os=darwin" {
		t.Errorf("reason = %q, want every label, as none has them all", got)
	}
	if got := d.reasons[3]; got != "daemon laptop does not have gpu=nvidia" {
		t.Errorf("reason = %q, want the bound daemon's lack", got)
	}
}

func TestGivenChildRequiringALabelWhenPlacedThenItGoesToItsParentsDaemonOnlyIfThatHasIt(t *testing.T) {
	s := labelled
	s.turns = []pendingTurn{
		requiring(placed(start(1, "on-parent", "vps", PriorityNormal), placementParent), Labels{"os": "linux"}),
		requiring(placed(start(2, "elsewhere", "vps", PriorityNormal), placementParent), Labels{"os": "darwin"}),
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{"on-parent@vps", "elsewhere@laptop"})
}

func TestGivenTaskRequiringALabelWhenOnlyAFullDaemonHasItThenItWaitsForThatDaemonsSlotAndYieldsFillerOnlyThere(t *testing.T) {
	s := labelled
	s.holders = []slotHolder{running("filler-vps", "vps", true, 2), running("filler-laptop", "laptop", true, 3)}
	s.turns = []pendingTurn{requiring(placed(start(1, "train", "laptop", PriorityNormal), placementAny), Labels{"gpu": "nvidia"})}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{})
	requireReason(t, d, 1, "slots: no connected daemon has a free slot")
	if got := yielded(d); len(got) != 1 || got[0] != "filler-vps" {
		t.Errorf("yielded %v, want the filler on vps only", got)
	}
}

func TestGivenAgentRequiringALabelWhenTheOwnerStartsATaskAsItThenTheTaskWaitsForADaemonWithIt(t *testing.T) {
	srv := startTestServer(t)
	createAgents(t, srv.store, Agent{Name: "trainer", Tools: []string{}, Priority: PriorityNormal, Requires: Labels{"gpu": "nvidia"}})
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/laptop/facts", `{"os":"darwin","gpu":"apple","slots":"2"}`)
	connect(t, srv, "laptop")

	task := queueTaskViaForm(t, srv, "", "Train.", url.Values{"agent": {"trainer"}})
	overridden := queueTaskViaForm(t, srv, "", "Train anywhere.", url.Values{"agent": {"trainer"}, "requires": {"os=darwin"}})

	if got := readTask(t, srv.store, task).Requires; got["gpu"] != "nvidia" {
		t.Errorf("requires = %v, want the agent's", got)
	}
	srv.pass(t)

	if state := readTask(t, srv.store, overridden).State; state == TaskQueued {
		t.Error("the task whose requires the owner overrode is still queued")
	}
	if q := readTask(t, srv.store, task).Queue; q == nil || q.Reason != "no daemon has gpu=nvidia" {
		t.Errorf("queue = %+v, want it waiting for gpu=nvidia", q)
	}
	requireContains(t, getPage(t, srv.url+"/tasks/"+string(task)), "<dt>Requires</dt><dd><code>gpu=nvidia</code></dd>", "no daemon has gpu=nvidia")

	if err := srv.store.setLabels(t.Context(), "laptop", Labels{"gpu": "nvidia"}); err != nil {
		t.Fatal(err)
	}
	srv.pass(t)
	if state := readTask(t, srv.store, task).State; state == TaskQueued {
		t.Error("the task is still queued once the owner labelled the laptop")
	}
}

func TestGivenSpawnWhenItGivesRequiresThenTheyReplaceTheAgentsAndOtherwiseTheChildHasTheAgents(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "parent", TaskRunning)
	createAgents(t, store, Agent{Name: "trainer", Tools: []string{}, Priority: PriorityNormal, Requires: Labels{"gpu": "nvidia"}})

	for child, requires := range map[protocol.TaskID]map[string]string{"agents": nil, "anywhere": {}, "linux": {"os": "linux"}} {
		if _, err := store.spawnTask(t.Context(), "laptop", "parent", child, protocol.Spawn{Prompt: "Train.", Agent: "trainer", Requires: requires}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := store.spawnTask(t.Context(), "laptop", "parent", "bad", protocol.Spawn{Prompt: "Train.", Requires: map[string]string{"gpu": "a b"}})

	for child, want := range map[protocol.TaskID]Labels{"agents": {"gpu": "nvidia"}, "anywhere": {}, "linux": {"os": "linux"}} {
		if got := readTask(t, store, child).Requires; !maps.Equal(got, want) {
			t.Errorf("%s requires %v, want %v", child, got, want)
		}
	}
	if !errors.Is(err, errRefused) {
		t.Errorf("spawn with a malformed label: err = %v, want a refusal", err)
	}
}

func TestGivenDaemonThatIsNotLoggedInWhenTurnsArePlacedThenItTakesNoneAndTheyWaitSayingSo(t *testing.T) {
	s := schedule{
		slots:  map[protocol.DaemonID]int{"laptop": 2, "vps": 2},
		labels: map[protocol.DaemonID]Labels{"laptop": {"login": "yes"}, "vps": {"login": "no", "gpu": "nvidia"}},
	}
	s.turns = []pendingTurn{
		start(1, "bound", "vps", PriorityNormal),
		requiring(placed(start(2, "gpu", "laptop", PriorityNormal), placementAny), Labels{"gpu": "nvidia"}),
		placed(start(3, "anywhere", "vps", PriorityNormal), placementParent),
		later(4, "follow-up", turnPrompt, TaskFinished, "vps"),
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{"anywhere@laptop"})
	for _, turn := range []uint64{1, 2, 4} {
		if got := d.reasons[turn]; got != "daemon vps is not logged in" {
			t.Errorf("turn %d waits because %q, want the login named", turn, got)
		}
	}
}

func TestGivenDaemonWithoutALoginFactWhenTurnsArePlacedThenItTakesThem(t *testing.T) {
	s := schedule{slots: map[protocol.DaemonID]int{"laptop": 1}, labels: map[protocol.DaemonID]Labels{"laptop": {"os": "linux"}}}
	s.turns = []pendingTurn{start(1, "task", "laptop", PriorityNormal)}

	requireStrings(t, "admitted", admitted(decide(s, testPolicy, schedNow)), []string{"task@laptop"})
}

func TestGivenDaemonThatLogsInWhenItsFactsSaySoThenTheWaitingTaskStarts(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/vps/facts", `{"os":"linux","login":"no","login_method":"none"}`)
	connect(t, srv, "vps")
	task := queueTaskViaForm(t, srv, "vps", "Work.", nil)

	srv.pass(t)

	if q := readTask(t, srv.store, task).Queue; q == nil || q.Reason != "daemon vps is not logged in" {
		t.Fatalf("queue = %+v, want it waiting for the login", q)
	}
	doRequest(t, http.MethodPut, srv.url+"/v1/daemons/vps/facts", `{"os":"linux","login":"yes","login_method":"claude.ai","account":"owner@example.com/org-1"}`)
	srv.pass(t)
	if state := readTask(t, srv.store, task).State; state == TaskQueued {
		t.Error("the task is still queued once the daemon logged in")
	}
}
