package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// advertise sets daemon's models label to models, separated by ';'.
func advertise(t *testing.T, store *Store, daemon protocol.DaemonID, models string) {
	t.Helper()
	if err := store.setLabels(t.Context(), daemon, Labels{protocol.FactModels: models}); err != nil {
		t.Fatal(err)
	}
}

func choosing(turn pendingTurn, models ...string) pendingTurn {
	turn.Models = models
	return turn
}

// admittedModels lists the turns d admits as "turn@daemon:model", in
// order.
func admittedModels(d decisions) []string {
	out := []string{}
	for _, a := range d.admit {
		out = append(out, string(a.turn.Task)+"@"+string(a.daemon)+":"+a.model)
	}
	return out
}

// fleet is a laptop running Claude Code with fable and sonnet, through
// its owner's label, and a vps running another harness that reports
// opus and sonnet as its models fact, with a free slot each.
func fleet() schedule {
	laptopFacts, laptopLabels := Labels{"harness": "claude-code"}, Labels{"models": "fable;sonnet"}
	vpsFacts := Labels{"harness": "pi", "models": "opus;sonnet"}
	return schedule{
		slots:  map[protocol.DaemonID]int{"laptop": 1, "vps": 1},
		labels: map[protocol.DaemonID]Labels{"laptop": Merge(laptopFacts, laptopLabels), "vps": vpsFacts},
		models: map[protocol.DaemonID][]string{"laptop": advertisedModels(laptopFacts, laptopLabels), "vps": advertisedModels(vpsFacts, nil)},
	}
}

func TestGivenAgentModelsWhenPlacedThenTheFirstOneADaemonProvidesIsChosenAndTheTaskGoesWhereItIs(t *testing.T) {
	s := fleet()
	s.turns = []pendingTurn{
		choosing(placed(start(1, "prefers-opus", "laptop", PriorityNormal), placementAny), "gpt-5", "opus", "fable"),
		choosing(placed(start(2, "prefers-fable", "laptop", PriorityNormal), placementAny), "fable", "opus"),
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admittedModels(d), []string{"prefers-opus@vps:opus", "prefers-fable@laptop:fable"})
}

func TestGivenQualifiedModelWhenPlacedThenOnlyADaemonRunningThatHarnessProvidesIt(t *testing.T) {
	s := fleet()
	s.turns = []pendingTurn{
		choosing(placed(start(1, "pi-sonnet", "laptop", PriorityNormal), placementAny), "pi:sonnet"),
		choosing(placed(start(2, "claude-opus", "laptop", PriorityNormal), placementAny), "claude-code:opus", "claude-code:fable"),
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admittedModels(d), []string{"pi-sonnet@vps:pi:sonnet", "claude-opus@laptop:claude-code:fable"})
}

func TestGivenModelsNoDaemonProvidesWhenPlacedThenTheTaskWaitsNamingThem(t *testing.T) {
	s := fleet()
	s.turns = []pendingTurn{
		choosing(placed(start(1, "any", "laptop", PriorityNormal), placementAny), "gpt-5", "pi:fable"),
		choosing(start(2, "bound", "laptop", PriorityNormal), "opus"),
		choosing(placed(start(3, "child", "laptop", PriorityNormal), placementParent), "o3", "gpt-5", "gemini"),
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{})
	for turn, want := range map[uint64]string{
		1: "waiting for a daemon with model gpt-5 or pi:fable",
		2: "daemon laptop does not have model opus",
		3: "waiting for a daemon with model o3, gpt-5 or gemini",
	} {
		if got := d.reasons[turn]; got != want {
			t.Errorf("turn %d waits because %q, want %q", turn, got, want)
		}
	}
}

func TestGivenChildChoosingAModelItsParentsDaemonLacksWhenPlacedThenItGoesToADaemonThatHasIt(t *testing.T) {
	s := fleet()
	s.turns = []pendingTurn{
		choosing(placed(start(1, "child", "laptop", PriorityNormal), placementParent), "opus", "fable"),
		placed(start(2, "fixed", "laptop", PriorityNormal), placementParent),
	}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admittedModels(d), []string{"child@vps:opus", "fixed@laptop:"})
}

func TestGivenModelOnlyAFullDaemonProvidesWhenPlacedThenTheTaskWaitsForItsSlotRatherThanTakeALaterModel(t *testing.T) {
	s := fleet()
	s.holders = []slotHolder{running("busy", "vps", true, 2)}
	s.turns = []pendingTurn{choosing(placed(start(1, "opus-first", "laptop", PriorityNormal), placementAny), "opus", "fable")}

	d := decide(s, testPolicy, schedNow)

	requireStrings(t, "admitted", admitted(d), []string{})
	requireReason(t, d, 1, "slots: no connected daemon has a free slot")
	if got := yielded(d); len(got) != 1 || got[0] != "busy" {
		t.Errorf("yielded %v, want the filler on vps, the daemon with opus", got)
	}
}

func TestGivenOwnersModelsLabelAndAdaptersFactWhenReadThenADaemonAdvertisesBoth(t *testing.T) {
	got := advertisedModels(Labels{"models": "opus;sonnet"}, Labels{"models": "fable;;opus"})

	if want := []string{"fable", "opus", "sonnet"}; !reflect.DeepEqual(got, want) {
		t.Errorf("models = %q, want %q", got, want)
	}
}

func TestGivenAgentWithModelsWhenItsTaskIsAdmittedThenTheChosenModelIsTheTasksAndTheStartsAndAGivenModelStands(t *testing.T) {
	srv := startTestServer(t)
	doRequest(t, http.MethodGet, srv.url+"/v1/daemons/laptop/acks", "")
	advertise(t, srv.store, "laptop", "sonnet;haiku")
	createAgents(t, srv.store, Agent{Name: "worker", Models: []string{"fable", "sonnet", "haiku"}, Tools: []string{}, Priority: PriorityNormal, Requires: Labels{}})
	chosen := postForTurn(t, srv.url+"/v1/tasks", `{"agent":"worker","prompt":"Work."}`, 201)
	given := postForTurn(t, srv.url+"/v1/tasks", `{"agent":"worker","prompt":"Work.","model":"opus"}`, 201)

	admitTurns(t, srv.store)

	for task, want := range map[protocol.TaskID]string{chosen.TaskID: "sonnet", given.TaskID: "opus"} {
		if detail := readTask(t, srv.store, task); detail.Model != want {
			t.Errorf("task %s model = %q, want %q", task, detail.Model, want)
		}
		var payload string
		err := srv.store.db.QueryRowContext(t.Context(), `SELECT payload FROM commands WHERE task_id = ? AND kind = ?`, string(task), string(protocol.CommandStartTask)).Scan(&payload)
		var start protocol.StartTask
		if err != nil || json.Unmarshal([]byte(payload), &start) != nil || start.Model != want {
			t.Errorf("task %s start = %s (%v), want model %q", task, payload, err, want)
		}
	}
	if models := readTask(t, srv.store, given.TaskID).Models; models != nil {
		t.Errorf("models of the task given a model = %q, want none", models)
	}
}

func TestGivenVersion17AgentsWhenMigratedThenEachModelBecomesItsOnlyModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.db")
	createVersionOneDatabase(t, path)
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys(1)"}).String())
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range append(migrations[:16:16],
		`INSERT INTO agents (name, description, system_prompt, model, tools, priority, filler, requires, created_at, updated_at)
			VALUES ('modelled', '', '', 'sonnet', '[]', 'normal', 0, '{}', '', ''), ('bare', '', '', '', '[]', 'normal', 0, '{}', '', '')`,
		`UPDATE schema_version SET version = 17`) {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	db.Close()

	store, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	agents, err := store.agents(t.Context())
	if err != nil || len(agents) != 2 {
		t.Fatalf("agents = %+v, %v", agents, err)
	}
	if agents[0].Name != "bare" || len(agents[0].Models) != 0 || !reflect.DeepEqual(agents[1].Models, []string{"sonnet"}) {
		t.Errorf("agents = %+v, want bare without models and modelled with [sonnet]", agents)
	}
}
