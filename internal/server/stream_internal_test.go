package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// openStreamOnce opens daemon's command stream with lastEventID and
// returns the status and, unless it is 200, the body; a stream that opens
// is closed at once.
func openStreamOnce(t *testing.T, srv testServer, daemon protocol.DaemonID, lastEventID string) (int, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.url+"/v1/daemons/"+string(daemon)+"/commands", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Last-Event-ID", lastEventID)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusOK {
		return http.StatusOK, ""
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

func requireRefused(t *testing.T, status int, body string, reason protocol.RefusalReason) protocol.StreamRefused {
	t.Helper()
	if status != http.StatusConflict {
		t.Fatalf("status = %d (%s), want 409", status, body)
	}
	var refused protocol.StreamRefused
	if err := json.Unmarshal([]byte(body), &refused); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if refused.Reason != reason {
		t.Fatalf("refused for %q (%s), want %q", refused.Reason, refused.Message, reason)
	}
	return refused
}

// listedRefusal returns the stream refusal the owner's daemon list shows
// for daemon, or nil.
func listedRefusal(t *testing.T, srv testServer, daemon protocol.DaemonID) *streamRefusal {
	t.Helper()
	var list []daemonView
	getJSON(t, srv.url+"/v1/daemons", &list)
	for _, view := range list {
		if view.ID == daemon {
			return view.StreamRefused
		}
	}
	t.Fatalf("daemon %s is not listed", daemon)
	return nil
}

func currentEpochOf(t *testing.T, store *Store) protocol.Epoch {
	t.Helper()
	ids, _ := lineage(t, store)
	return protocol.Epoch(ids[len(ids)-1])
}

func TestGivenAPositionInTheCurrentEpochWhenTheStreamOpensThenTheCommandsPastItAreSent(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	prompt, err := srv.store.issueCommand(t.Context(), "task-1", protocol.CommandPrompt, json.RawMessage(`{"text":"go on"}`))
	if err != nil {
		t.Fatal(err)
	}
	epoch := currentEpochOf(t, srv.store)

	events := openCommandStream(t, srv, "laptop", protocol.CommandPosition{Epoch: epoch, ID: prompt.ID - 1}.String())

	if got := receiveCommand(t, events); got.ID != prompt.ID || got.Epoch != epoch {
		t.Errorf("first command = %s, want the prompt %s", got.Position(), prompt.Position())
	}
}

func TestGivenAPositionPastTheLastCommandIssuedInTheCurrentEpochWhenTheStreamOpensThenItIsRefusedAsDaemonAheadAndListed(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	epoch := currentEpochOf(t, srv.store)
	before := time.Now()

	status, body := openStreamOnce(t, srv, "laptop", protocol.CommandPosition{Epoch: epoch, ID: 99}.String())

	refused := requireRefused(t, status, body, protocol.RefusedDaemonAhead)
	if !strings.Contains(refused.Message, "99") || !strings.Contains(refused.Message, string(epoch)) {
		t.Errorf("message %q does not name the daemon's position", refused.Message)
	}
	listed := listedRefusal(t, srv, "laptop")
	if listed == nil || listed.Reason != protocol.RefusedDaemonAhead || listed.At.Before(before.Add(-time.Second)) {
		t.Errorf("the daemon list shows %+v, want the daemon_ahead refusal just now", listed)
	}
	if page := getPage(t, srv.url+"/"); !strings.Contains(page, "stream refused: daemon_ahead") {
		t.Error("the dashboard does not show the refusal")
	}
}

func TestGivenARefusedStreamWhenALaterOneOpensThenTheListNoLongerShowsTheRefusal(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	epoch := currentEpochOf(t, srv.store)
	status, body := openStreamOnce(t, srv, "laptop", protocol.CommandPosition{Epoch: epoch, ID: 99}.String())
	requireRefused(t, status, body, protocol.RefusedDaemonAhead)

	if status, body := openStreamOnce(t, srv, "laptop", protocol.CommandPosition{Epoch: epoch, ID: 1}.String()); status != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200", status, body)
	}

	if listed := listedRefusal(t, srv, "laptop"); listed != nil {
		t.Errorf("the daemon list still shows %+v", listed)
	}
}

func TestGivenAnEpochOutsideTheLineageWhenTheStreamOpensThenItIsRefusedAsUnknownLineageNamingTheCurrentEpoch(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	epoch := currentEpochOf(t, srv.store)

	status, body := openStreamOnce(t, srv, "laptop", "0123456789abcdef0123456789abcdef:1")

	refused := requireRefused(t, status, body, protocol.RefusedUnknownEpoch)
	if !strings.Contains(refused.Message, string(epoch)) {
		t.Errorf("message %q does not name the current epoch %s", refused.Message, epoch)
	}
	if listed := listedRefusal(t, srv, "laptop"); listed == nil || listed.Reason != protocol.RefusedUnknownEpoch {
		t.Errorf("the daemon list shows %+v, want the unknown_epoch refusal", listed)
	}
}

// restoredServer returns a server over a store restored from a backup of
// a database that issued the start of task-1 and a prompt to laptop, and
// that has since issued a pause and a resume in its own epoch. It returns
// the backup's epoch and the commands in order.
func restoredServer(t *testing.T) (testServer, protocol.Epoch, []protocol.Command) {
	t.Helper()
	store, path := openTestStore(t)
	seedTask(t, store, "laptop", "task-1")
	if _, err := store.issueCommand(t.Context(), "task-1", protocol.CommandPrompt, json.RawMessage(`{"text":"go on"}`)); err != nil {
		t.Fatal(err)
	}
	backupEpoch := currentEpochOf(t, store)
	store.Close()
	markRestored(t, path)
	restored := reopen(t, path)
	for _, kind := range []protocol.CommandKind{protocol.CommandPause, protocol.CommandResume} {
		if _, err := restored.issueCommand(t.Context(), "task-1", kind, nil); err != nil {
			t.Fatal(err)
		}
	}
	commands, err := restored.commandsAfter(t.Context(), "laptop", protocol.CommandPosition{})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 4 {
		t.Fatalf("commands = %+v, want 4", commands)
	}
	return startTestServerOn(t, restored, Options{}), backupEpoch, commands
}

func TestGivenAPositionInAnEarlierEpochWhenTheStreamOpensThenItGoesOnFromThereThroughTheLaterEpochs(t *testing.T) {
	srv, backupEpoch, commands := restoredServer(t)
	newEpoch := currentEpochOf(t, srv.store)

	events := openCommandStream(t, srv, "laptop", protocol.CommandPosition{Epoch: backupEpoch, ID: commands[0].ID}.String())

	var got []string
	for range 3 {
		got = append(got, receiveCommand(t, events).Position().String())
	}
	want := []string{commands[1].Position().String(), commands[2].Position().String(), commands[3].Position().String()}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("sent %q, want %q", got, want)
	}
	if commands[1].Epoch != backupEpoch || commands[2].Epoch != newEpoch {
		t.Errorf("epochs %s, %s; want the backup's then the new one", commands[1].Epoch, commands[2].Epoch)
	}
}

// The daemon applied commands the lost database issued after its last
// backup, so its id in the backup's epoch is past every id the restored
// database holds of that epoch, and may be past ids the new epoch has
// issued since.
func TestGivenAPositionInAnEarlierEpochPastWhatTheBackupHeldWhenTheStreamOpensThenEveryCommandOfTheLaterEpochsIsSent(t *testing.T) {
	srv, backupEpoch, commands := restoredServer(t)

	events := openCommandStream(t, srv, "laptop", protocol.CommandPosition{Epoch: backupEpoch, ID: commands[3].ID + 10}.String())

	first, second := receiveCommand(t, events), receiveCommand(t, events)
	if first.Position() != commands[2].Position() || second.Position() != commands[3].Position() {
		t.Errorf("sent %s and %s, want the new epoch's %s and %s", first.Position(), second.Position(), commands[2].Position(), commands[3].Position())
	}
	if listed := listedRefusal(t, srv, "laptop"); listed != nil {
		t.Errorf("the daemon list shows %+v, want no refusal", listed)
	}
}

// A daemon that last applied a command before commands had epochs sends a
// bare id. Every such command is in the first epoch.
func TestGivenABareIDWhenTheStreamOpensThenItIsAPositionInTheFirstEpoch(t *testing.T) {
	t.Run("Given the first epoch is the current one when the id was issued then the commands past it are sent", func(t *testing.T) {
		srv := startTestServer(t)
		seedTask(t, srv.store, "laptop", "task-1")
		prompt, err := srv.store.issueCommand(t.Context(), "task-1", protocol.CommandPrompt, json.RawMessage(`{"text":"go on"}`))
		if err != nil {
			t.Fatal(err)
		}

		events := openCommandStream(t, srv, "laptop", "1")

		if got := receiveCommand(t, events); got.Position() != prompt.Position() {
			t.Errorf("first command = %s, want %s", got.Position(), prompt.Position())
		}
	})
	t.Run("Given the first epoch is the current one when the id is past the last issued then it is refused as daemon_ahead", func(t *testing.T) {
		srv := startTestServer(t)
		seedTask(t, srv.store, "laptop", "task-1")

		status, body := openStreamOnce(t, srv, "laptop", "99")

		requireRefused(t, status, body, protocol.RefusedDaemonAhead)
	})
	t.Run("Given a restored database when the id is past the backup then the later epochs are sent", func(t *testing.T) {
		srv, _, commands := restoredServer(t)

		events := openCommandStream(t, srv, "laptop", "99")

		if got := receiveCommand(t, events); got.Position() != commands[2].Position() {
			t.Errorf("first command = %s, want the new epoch's first, %s", got.Position(), commands[2].Position())
		}
	})
}
