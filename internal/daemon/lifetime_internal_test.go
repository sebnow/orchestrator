package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// finishTurn answers the process's next prompt with a turn that ends
// reporting session, and returns the prompt.
func finishTurn(t *testing.T, proc *fakeProcess, session string) fakeInput {
	t.Helper()
	in := proc.nextInput(t)
	if in.kind != "prompt" {
		t.Fatalf("input = %+v, want a prompt", in)
	}
	proc.emit(harness.Output{Line: []byte(`{"type":"result"}`), TurnEnded: true, Answering: []string{in.id}, SessionID: session})
	return in
}

// expectExit waits for the process's input to close, which ends the fake.
func expectExit(t *testing.T, proc *fakeProcess) {
	t.Helper()
	if in := proc.nextInput(t); in.kind != "close" {
		t.Fatalf("input = %+v, want the input closed", in)
	}
}

func harnessExits(events []protocol.Event) int {
	count := 0
	for _, event := range events {
		if event.Kind == protocol.KindHarnessExited {
			count++
		}
	}
	return count
}

// waitForCount waits until the server holds n events of kind for task,
// and returns them all.
func (f *serverFixture) waitForCount(t *testing.T, task protocol.TaskID, kind protocol.Kind, n int) []protocol.Event {
	t.Helper()
	var events []protocol.Event
	eventually(t, fmt.Sprintf("%d %s events", n, kind), func() bool {
		events = f.events(t, task)
		count := 0
		for _, event := range events {
			if event.Kind == kind {
				count++
			}
		}
		return count >= n
	})
	return events
}

func journalGone(t *testing.T, stateDir string, task protocol.TaskID) func() bool {
	return func() bool {
		_, err := os.Stat(JournalPath(stateDir, task))
		return errors.Is(err, fs.ErrNotExist)
	}
}

func TestGivenTurnThatEndsWithNothingOutstandingWhenItEndsThenTheHarnessExitsAndTheServerHearsItCleanly(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)

	finishTurn(t, proc, "session-1")

	expectExit(t, proc)
	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	assertContiguous(t, events)
	if string(events[len(events)-1].Payload) != `{"exit_code":0}` {
		t.Errorf("events: %s", describe(events))
	}
	eventually(t, "the journal to be deleted", journalGone(t, d.stateDir, task))
	rec, ok := mustLoadState(t, d.stateDir).record(task)
	if !ok || rec.Session != "session-1" || rec.Seq != uint64(len(events)) || !rec.resumable() {
		t.Errorf("record = %+v, %v", rec, ok)
	}
}

func TestGivenFollowUpWhileATurnRunsWhenTheFirstTurnEndsThenTheProcessStaysForTheFollowUpsTurn(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)
	first := proc.nextInput(t)
	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "Also this."})
	second := proc.nextInput(t)

	proc.emit(harness.Output{Line: []byte(`{"type":"result"}`), TurnEnded: true, Answering: []string{first.id}})
	proc.noInput(t)
	proc.emit(harness.Output{Line: []byte(`{"type":"result"}`), TurnEnded: true, Answering: []string{second.id}})

	expectExit(t, proc)
	srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
}

func TestGivenFinishedTaskWhenTheOwnerPromptsThenANewProcessResumesItsSessionInTheSameWorkspaceAndSeqsContinue(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{
		Prompt: "The codeword is MARMALADE.", Model: "fake-model", SystemPrompt: "Be brief.", PauseLimits: testPauseLimits,
	})
	first := d.nextProcess(t)
	finishTurn(t, first, "session-1")
	expectExit(t, first)
	srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	eventually(t, "the journal to be deleted", journalGone(t, d.stateDir, task))

	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "What is the codeword?"})

	second := d.nextProcess(t)
	spec := second.spec
	if spec.Resume != "session-1" || spec.Workdir != first.spec.Workdir || spec.Model != "fake-model" ||
		spec.SystemPrompt != first.spec.SystemPrompt || !strings.HasSuffix(spec.SystemPrompt, "\n\nBe brief.") {
		t.Errorf("second spec = %+v, first workdir %s", spec, first.spec.Workdir)
	}
	if in := finishTurn(t, second, "session-1"); in.text != "What is the codeword?" {
		t.Errorf("prompt = %q", in.text)
	}
	expectExit(t, second)
	events := srv.waitForCount(t, task, protocol.KindHarnessExited, 2)
	assertContiguous(t, events)
	var kinds []protocol.Kind
	for _, event := range events {
		if event.Kind != protocol.KindHarnessOutput {
			kinds = append(kinds, event.Kind)
		}
	}
	want := []protocol.Kind{protocol.KindHarnessStarted, protocol.KindHarnessExited, protocol.KindHarnessStarted, protocol.KindHarnessExited}
	if strings.Join(kindStrings(kinds), ",") != strings.Join(kindStrings(want), ",") {
		t.Errorf("control events = %v, want %v", kinds, want)
	}
}

func kindStrings(kinds []protocol.Kind) []string {
	out := make([]string, len(kinds))
	for idx, kind := range kinds {
		out[idx] = string(kind)
	}
	return out
}

func TestGivenPromptRightAfterATurnEndsWhenTheProcessIsStillExitingThenTheNextProcessGetsIt(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	first := d.nextProcess(t)
	first.endOnClose = false
	finishTurn(t, first, "session-1")
	expectExit(t, first)

	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "And then?"})
	first.noInput(t)
	first.end(protocol.HarnessExited{})

	second := d.nextProcess(t)
	if in := second.nextInput(t); in.kind != "prompt" || in.text != "And then?" || second.spec.Resume != "session-1" {
		t.Errorf("input = %+v, spec %+v", in, second.spec)
	}
}

func TestGivenPauseThatSettlesWhenTheOwnerResumesThenANewProcessResumesWithTheStopNote(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do three steps.", PauseLimits: testPauseLimits})
	first := d.nextProcess(t)
	prompt := first.nextInput(t)
	srv.command(t, task, protocol.CommandPause, nil)
	pauseRequest := first.nextInput(t)
	acknowledgePauseThroughGateway(t, first, "Stopped after step 1.")
	first.emit(harness.Output{Line: []byte(`{"type":"result"}`), TurnEnded: true, Answering: []string{prompt.id, pauseRequest.id}, SessionID: "session-1"})
	expectExit(t, first)
	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	if last := events[len(events)-2:]; last[0].Kind != protocol.KindPauseSettled {
		t.Fatalf("events: %s", describe(events))
	}

	srv.command(t, task, protocol.CommandResume, nil)

	second := d.nextProcess(t)
	in := second.nextInput(t)
	if want := resumePrompt + " Your note when you stopped: Stopped after step 1."; in.text != want || second.spec.Resume != "session-1" {
		t.Errorf("input = %+v, want %q; spec %+v", in, want, second.spec)
	}
	second.emit(harness.Output{Line: []byte(`{"type":"result"}`), TurnEnded: true, Answering: []string{in.id}, SessionID: "session-1"})
	expectExit(t, second)
	srv.waitForCount(t, task, protocol.KindHarnessExited, 2)
	eventually(t, "the record to show the task no longer paused", func() bool {
		rec, _ := mustLoadState(t, d.stateDir).record(task)
		return !rec.Paused && rec.StopNote == "" && rec.resumable()
	})
}

func TestGivenTaskBetweenProcessesWhenTheOwnerStopsItThenItIsForgottenAndAPromptStartsNothing(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)
	finishTurn(t, proc, "session-1")
	expectExit(t, proc)
	srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	eventually(t, "the journal to be deleted", journalGone(t, d.stateDir, task))

	srv.command(t, task, protocol.CommandStop, nil)

	events := srv.waitForCount(t, task, protocol.KindHarnessExited, 2)
	assertContiguous(t, events)
	if last := events[len(events)-1]; string(last.Payload) != `{"exit_code":-1,"error":"stopped with no process running"}` {
		t.Errorf("events: %s", describe(events))
	}
	eventually(t, "the task to be forgotten", func() bool { return !mustLoadState(t, d.stateDir).known(task) })
	next := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Next.", PauseLimits: testPauseLimits})
	if proc := d.nextProcess(t); !strings.HasSuffix(proc.spec.Workdir, string(next)) {
		t.Errorf("a harness started in %s, want only the next task's", proc.spec.Workdir)
	}
}

func TestGivenRunningTaskWhenTheOwnerStopsItThenItIsForgottenOnceTheServerHoldsItsExit(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)
	proc.nextInput(t)
	proc.emit(harness.Output{Line: []byte(`{"type":"system","subtype":"init"}`), SessionID: "session-1"})

	srv.command(t, task, protocol.CommandStop, nil)

	srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	eventually(t, "the task to be forgotten", func() bool { return !mustLoadState(t, d.stateDir).known(task) })
	eventually(t, "the journal to be deleted", journalGone(t, d.stateDir, task))
}

func TestGivenTaskWhoseHarnessFailedWhenTheOwnerPromptsThenNoProcessStarts(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)
	proc.nextInput(t)
	proc.emit(harness.Output{Line: []byte(`{"type":"system","subtype":"init"}`), SessionID: "session-1"})
	proc.end(protocol.HarnessExited{ExitCode: 1})
	srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	eventually(t, "the task to be forgotten", func() bool { return !mustLoadState(t, d.stateDir).known(task) })

	srv.tryCommand(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "Try again."})

	next := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Next.", PauseLimits: testPauseLimits})
	if proc := d.nextProcess(t); !strings.HasSuffix(proc.spec.Workdir, string(next)) {
		t.Errorf("a harness started in %s, want only the next task's", proc.spec.Workdir)
	}
}

func TestGivenFinishedTaskWhenTheDaemonRestartsAndTheOwnerPromptsThenItResumesWithItsSeqsContinuing(t *testing.T) {
	srv := startServer(t)
	stateDir := t.TempDir()
	first := runDaemon(t, srv.url, stateDir)
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := first.nextProcess(t)
	finishTurn(t, proc, "session-1")
	expectExit(t, proc)
	srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	eventually(t, "the journal to be deleted", journalGone(t, stateDir, task))
	first.stop(t)

	second := runDaemon(t, srv.url, stateDir)
	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "Carry on."})

	resumed := second.nextProcess(t)
	if resumed.spec.Resume != "session-1" {
		t.Errorf("spec = %+v", resumed.spec)
	}
	finishTurn(t, resumed, "session-1")
	expectExit(t, resumed)
	events := srv.waitForCount(t, task, protocol.KindHarnessExited, 2)
	assertContiguous(t, events)
	var exit protocol.HarnessExited
	json.Unmarshal(events[len(events)-1].Payload, &exit)
	if harnessExits(events) != 2 || exit.Error != "" {
		t.Errorf("events: %s", describe(events))
	}
}

// tryCommand issues a command that the server may refuse.
func (f *serverFixture) tryCommand(t *testing.T, task protocol.TaskID, kind protocol.CommandKind, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	f.try(t, "POST", "/v1/tasks/"+string(task)+"/commands", map[string]any{"kind": kind, "payload": json.RawMessage(raw)})
}

func TestGivenResumedTaskWhoseJournalTheServerDoesNotHoldYetWhenPromptedAgainThenItsThirdProcessContinuesTheJournal(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "One.", PauseLimits: testPauseLimits})
	first := d.nextProcess(t)
	finishTurn(t, first, "session-1")
	expectExit(t, first)
	srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	eventually(t, "the journal to be deleted", journalGone(t, d.stateDir, task))
	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "Two."})
	second := d.nextProcess(t)
	srv.down.Store(true)
	finishTurn(t, second, "session-1")
	expectExit(t, second)
	eventually(t, "the second process to be recorded", func() bool {
		rec, _ := mustLoadState(t, d.stateDir).record(task)
		return rec.Seq > 4
	})

	srv.down.Store(false)
	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "Three."})

	third := d.nextProcess(t)
	if in := finishTurn(t, third, "session-1"); in.text != "Three." {
		t.Errorf("prompt = %q", in.text)
	}
	expectExit(t, third)
	events := srv.waitForCount(t, task, protocol.KindHarnessExited, 3)
	assertContiguous(t, events)
}

func TestGivenContinuationJournalLeftByACrashWhenTheDaemonRestartsThenTheTaskIsPausedAndResumeContinuesItsSession(t *testing.T) {
	srv := startServer(t)
	proxy := startProxy(t, srv.url)
	stateDir := t.TempDir()
	first := runDaemon(t, proxy.url(), stateDir)
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "One.", PauseLimits: testPauseLimits})
	proc := first.nextProcess(t)
	finishTurn(t, proc, "session-1")
	expectExit(t, proc)
	srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	eventually(t, "the journal to be deleted", journalGone(t, stateDir, task))
	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "Two."})
	resumed := first.nextProcess(t)
	resumed.nextInput(t)
	srv.waitForCount(t, task, protocol.KindHarnessStarted, 2)
	crashed := t.TempDir()
	if err := os.CopyFS(crashed, os.DirFS(stateDir)); err != nil {
		t.Fatal(err)
	}
	proxy.cutAll()
	first.stop(t)

	second := runDaemon(t, srv.url, crashed)

	events := srv.waitForCount(t, task, protocol.KindHarnessExited, 2)
	assertContiguous(t, events)
	var exit protocol.HarnessExited
	json.Unmarshal(events[len(events)-1].Payload, &exit)
	if exit.Error != restartError {
		t.Errorf("events: %s", describe(events))
	}
	srv.waitForState(t, task, "paused")

	srv.command(t, task, protocol.CommandResume, nil)

	again := second.nextProcess(t)
	if again.spec.Resume != "session-1" {
		t.Errorf("spec = %+v, want session-1 resumed", again.spec)
	}
	if in := finishTurn(t, again, "session-1"); in.text != resumePrompt+cutShortNote {
		t.Errorf("resume prompt = %q", in.text)
	}
	expectExit(t, again)
	srv.waitForState(t, task, "finished")
	assertContiguous(t, srv.events(t, task))
}

func TestGivenFirstTurnThatReportedItsSessionWhenTheDaemonDiesAndTheOwnerPromptsThenThatSessionIsResumed(t *testing.T) {
	srv := startServer(t)
	proxy := startProxy(t, srv.url)
	stateDir := t.TempDir()
	first := runDaemon(t, proxy.url(), stateDir)
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "One.", PauseLimits: testPauseLimits})
	proc := first.nextProcess(t)
	proc.nextInput(t)
	proc.emit(harness.Output{Line: []byte(`{"type":"system","subtype":"init"}`), SessionID: "session-1"})
	eventually(t, "the session to be recorded", func() bool {
		rec, _ := mustLoadState(t, stateDir).record(task)
		return rec.Session == "session-1"
	})
	crashed := t.TempDir()
	if err := os.CopyFS(crashed, os.DirFS(stateDir)); err != nil {
		t.Fatal(err)
	}
	proxy.cutAll()
	first.stop(t)
	second := runDaemon(t, srv.url, crashed)
	srv.waitForState(t, task, "paused")

	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "Carry on."})

	resumed := second.nextProcess(t)
	if resumed.spec.Resume != "session-1" {
		t.Errorf("spec = %+v, want session-1 resumed", resumed.spec)
	}
	if in := resumed.nextInput(t); in.text != "Carry on." {
		t.Errorf("prompt = %q", in.text)
	}
}

func TestGivenProcessThatDoesNotExitAfterItsTurnWhenAPromptWaitsForItThenItIsKilledAndTheServerHearsWhy(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)
	proc.endOnClose = false
	finishTurn(t, proc, "session-1")
	expectExit(t, proc)

	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "And then?"})

	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	if last := events[len(events)-1]; string(last.Payload) != `{"exit_code":-1,"error":"signal: killed"}` {
		t.Errorf("events: %s", describe(events))
	}
}

// stopDuringTurn shuts the daemon d down while proc's turn runs, and has
// the harness exit with own once the daemon closes its input.
func stopDuringTurn(t *testing.T, d *daemonFixture, proc *fakeProcess, own protocol.HarnessExited) {
	t.Helper()
	proc.endOnClose = false
	stopped := make(chan struct{})
	go func() {
		d.stop(t)
		close(stopped)
	}()
	if in := proc.nextInput(t); in.kind != "interrupt" {
		t.Fatalf("input = %+v, want an interrupt", in)
	}
	if in := proc.nextInput(t); in.kind != "close" {
		t.Fatalf("input = %+v, want the input closed", in)
	}
	proc.end(own)
	<-stopped
}

func TestGivenRunningTurnWhenTheDaemonShutsDownThenTheTaskIsPausedAndAfterARestartResumeContinuesItsSession(t *testing.T) {
	srv := startServer(t)
	stateDir := t.TempDir()
	first := runDaemon(t, srv.url, stateDir)
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := first.nextProcess(t)
	proc.nextInput(t)
	proc.emit(harness.Output{Line: []byte(`{"type":"system","subtype":"init"}`), SessionID: "session-1"})
	srv.waitForEvent(t, task, "seq 2", hasSeq(2))

	// Claude Code exits 1 after an interrupted turn.
	stopDuringTurn(t, first, proc, protocol.HarnessExited{ExitCode: 1, Stderr: "tail"})

	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	if last := events[len(events)-1]; string(last.Payload) != `{"exit_code":-1,"error":"`+stopError+`","stderr":"tail"}` {
		t.Errorf("events: %s", describe(events))
	}
	srv.waitForState(t, task, "paused")
	if rec, _ := mustLoadState(t, stateDir).record(task); rec.Ended || !rec.CutShort || rec.Session != "session-1" {
		t.Errorf("record = %+v", rec)
	}

	second := runDaemon(t, srv.url, stateDir)
	srv.command(t, task, protocol.CommandResume, nil)

	resumed := second.nextProcess(t)
	if resumed.spec.Resume != "session-1" {
		t.Errorf("spec = %+v, want session-1 resumed", resumed.spec)
	}
	if in := finishTurn(t, resumed, "session-1"); in.text != resumePrompt+cutShortNote {
		t.Errorf("resume prompt = %q", in.text)
	}
	expectExit(t, resumed)
	srv.waitForState(t, task, "finished")
	assertContiguous(t, srv.events(t, task))
}

func TestGivenFirstTurnBeforeASessionWhenTheDaemonShutsDownThenResumeStartsANewSession(t *testing.T) {
	srv := startServer(t)
	stateDir := t.TempDir()
	first := runDaemon(t, srv.url, stateDir)
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := first.nextProcess(t)
	proc.nextInput(t)

	stopDuringTurn(t, first, proc, protocol.HarnessExited{ExitCode: 1})

	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	if last := events[len(events)-1]; string(last.Payload) != `{"exit_code":-1,"error":"`+stopNoSessionError+`"}` {
		t.Errorf("events: %s", describe(events))
	}
	srv.waitForState(t, task, "paused")
}

func TestGivenHarnessThatStartsWhenItsProcessEndsThenTheRecordHeldItsPIDAndStartTimeUntilThen(t *testing.T) {
	srv := startServer(t)
	stateDir := t.TempDir()
	d := runDaemon(t, srv.url, stateDir)
	d.processes.run(4242, "Thu Oct  8 18:10:35 2026", -1)
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)

	want := harnessProcess{PID: 4242, Started: "Thu Oct  8 18:10:35 2026"}
	eventually(t, "the harness to be recorded", func() bool {
		rec, _ := mustLoadState(t, stateDir).record(task)
		return rec.Harness != nil && *rec.Harness == want
	})
	finishTurn(t, proc, "session-1")
	expectExit(t, proc)
	eventually(t, "the harness to be forgotten", func() bool {
		rec, _ := mustLoadState(t, stateDir).record(task)
		return rec.Harness == nil && !rec.Running
	})
}

func TestGivenDaemonThatDiedLeavingItsHarnessRunningWhenANewOneStartsThenItKillsTheHarnessBeforeThePauseIsReported(t *testing.T) {
	srv := startServer(t)
	proxy := startProxy(t, srv.url)
	stateDir := t.TempDir()
	first := runDaemon(t, proxy.url(), stateDir)
	first.processes.run(4242, "Thu Oct  8 18:10:35 2026", -1)
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := first.nextProcess(t)
	proc.nextInput(t)
	srv.waitForEvent(t, task, "harness_started", isKind(protocol.KindHarnessStarted))
	crashed := t.TempDir()
	if err := os.CopyFS(crashed, os.DirFS(stateDir)); err != nil {
		t.Fatal(err)
	}
	proxy.cutAll()
	first.stop(t)
	// The harness outlived the daemon that started it.
	orphans := newFakeProcesses()
	orphans.run(4242, "Thu Oct  8 18:10:35 2026", -1)

	second := runDaemonWithProcesses(t, srv.url, crashed, nil, orphans)

	srv.waitForState(t, task, "paused")
	if killed := second.processes.killedPIDs(); !slices.Equal(killed, []int{4242}) {
		t.Errorf("killed %v, want the harness 4242", killed)
	}
	if rec, _ := mustLoadState(t, crashed).record(task); rec.Harness != nil {
		t.Errorf("record = %+v", rec)
	}
}
