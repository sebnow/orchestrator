package daemon

import (
	"testing"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// interruptedTurn starts a task whose harness reports session-1, has the
// owner interrupt its turn, and returns the task, its process and the
// first prompt's input, once the harness has had the interrupt. The
// process does not exit when its input closes.
func interruptedTurn(t *testing.T, srv *serverFixture, d *daemonFixture) (protocol.TaskID, *fakeProcess, fakeInput) {
	t.Helper()
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)
	proc.endOnClose = false
	first := proc.nextInput(t)
	proc.emit(harness.Output{Line: []byte(`{"type":"system","subtype":"init"}`), SessionID: "session-1"})
	srv.waitForEvent(t, task, "the init line", hasSeq(2))

	srv.command(t, task, protocol.CommandInterrupt, nil)

	if in := proc.nextInput(t); in.kind != "interrupt" {
		t.Fatalf("input = %+v, want an interrupt", in)
	}
	return task, proc, first
}

// errorResult ends the turn answering ids with an error, as Claude Code
// ends an interrupted turn.
func errorResult(ids ...string) harness.Output {
	return harness.Output{Line: []byte(`{"type":"result","is_error":true}`), TurnEnded: true, Answering: ids}
}

func TestGivenRunningTurnWhenTheOwnerInterruptsItAndTheHarnessExitsWithAnErrorThenTheTaskIsPausedAndResumeSaysWhy(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task, proc, first := interruptedTurn(t, srv, d)

	proc.emit(errorResult(first.id))
	expectExit(t, proc)
	proc.end(protocol.HarnessExited{ExitCode: 1})

	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	if last := events[len(events)-1]; string(last.Payload) != `{"exit_code":-1,"error":"interrupted by the owner"}` {
		t.Errorf("events: %s", describe(events))
	}
	srv.waitForState(t, task, "paused")

	srv.command(t, task, protocol.CommandResume, nil)

	next := d.nextProcess(t)
	if next.spec.Resume != "session-1" {
		t.Errorf("resumed session %q, want session-1", next.spec.Resume)
	}
	if in := next.nextInput(t); in.text != resumePrompt+interruptedNote {
		t.Errorf("resume prompt = %q, want it to say the owner interrupted the last turn", in.text)
	}
}

func TestGivenInterruptedTurnWhenTheOwnersNextPromptsTurnFailsThenTheTaskFails(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task, proc, first := interruptedTurn(t, srv, d)
	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "Try something else."})
	srv.waitForEvent(t, task, "prompt_held", isKind(protocol.KindPromptHeld))

	proc.emit(errorResult(first.id))
	second := proc.nextInput(t)
	proc.emit(errorResult(second.id))
	expectExit(t, proc)
	proc.end(protocol.HarnessExited{ExitCode: 1})

	srv.waitForState(t, task, "failed")
	events := srv.events(t, task)
	if last := events[len(events)-1]; string(last.Payload) != `{"exit_code":1}` {
		t.Errorf("events: %s", describe(events))
	}
}
