package daemon

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// runningTurn starts a task whose harness reports session-1 and returns
// the task, its process and the first prompt's input, while the turn
// runs.
func runningTurn(t *testing.T, srv *serverFixture, d *daemonFixture) (protocol.TaskID, *fakeProcess, fakeInput) {
	t.Helper()
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Count to twenty.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)
	first := proc.nextInput(t)
	proc.emit(harness.Output{Line: []byte(`{"type":"system","subtype":"init"}`), SessionID: "session-1"})
	srv.waitForEvent(t, task, "the init line", hasSeq(2))
	return task, proc, first
}

// heldPromptID returns the id of the prompt command the daemon holds for
// task, as the server lists it.
func heldPromptID(t *testing.T, srv *serverFixture, task protocol.TaskID) uint64 {
	t.Helper()
	srv.waitForEvent(t, task, "prompt_held", isKind(protocol.KindPromptHeld))
	var detail struct {
		Queued []struct {
			Prompt uint64 `json:"prompt"`
		} `json:"queued"`
	}
	srv.call(t, http.MethodGet, "/v1/tasks/"+string(task), nil, &detail)
	if len(detail.Queued) != 1 || detail.Queued[0].Prompt == 0 {
		t.Fatalf("queued = %+v, want one held prompt", detail.Queued)
	}
	return detail.Queued[0].Prompt
}

func TestGivenRunningTurnWhenTheOwnerSteersThenTheTurnIsInterruptedAndTheMessageIsTheNextTurnsPromptInTheSameProcess(t *testing.T) {
	for name, interrupted := range map[string]func(fakeInput) harness.Output{
		"result answers the first prompt": func(first fakeInput) harness.Output { return errorResult(first.id) },
		"result answers nothing":          func(fakeInput) harness.Output { return errorResult() },
	} {
		t.Run(name, func(t *testing.T) {
			srv := startServer(t)
			d := runDaemon(t, srv.url, t.TempDir())
			task, proc, first := runningTurn(t, srv, d)

			srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "Stop at ten.", Steer: true})

			if in := proc.nextInput(t); in.kind != "interrupt" {
				t.Fatalf("input = %+v, want the interrupt first", in)
			}
			steer := proc.nextInput(t)
			if steer.kind != "prompt" || steer.text != "Stop at ten." {
				t.Fatalf("input = %+v, want the steering prompt", steer)
			}
			proc.emit(interrupted(first))
			proc.noInput(t)
			proc.emit(harness.Output{Line: []byte(`{"type":"result","subtype":"success"}`), TurnEnded: true, Answering: []string{steer.id}, SessionID: "session-1"})
			expectExit(t, proc)

			events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
			if last := events[len(events)-1]; string(last.Payload) != `{"exit_code":0}` {
				t.Errorf("events: %s, want the harness's own clean exit", describe(events))
			}
			srv.waitForState(t, task, "finished")
		})
	}
}

func TestGivenTaskWithNoProcessWhenTheOwnerSteersThenTheMessageResumesItsSession(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task, proc, first := runningTurn(t, srv, d)
	proc.emit(harness.Output{Line: []byte(`{"type":"result"}`), TurnEnded: true, Answering: []string{first.id}, SessionID: "session-1"})
	expectExit(t, proc)

	srv.waitForState(t, task, "finished")

	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "Carry on.", Steer: true})

	resumed := d.nextProcess(t)
	if in := resumed.nextInput(t); in.kind != "prompt" || in.text != "Carry on." || resumed.spec.Resume != "session-1" {
		t.Errorf("input %+v in spec %+v, want the prompt resuming session-1", in, resumed.spec)
	}
}

func TestGivenHeldPromptWhenTheOwnerWithdrawsItBeforeTheTurnEndsThenItIsNeverSentAndTheProcessEndsWithTheTurn(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task, proc, first := runningTurn(t, srv, d)
	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "After this, the next."})
	held := heldPromptID(t, srv, task)

	srv.command(t, task, protocol.CommandWithdraw, protocol.Withdraw{Prompt: held})
	srv.waitForEvent(t, task, "prompt_released", isKind(protocol.KindPromptReleased))
	proc.emit(harness.Output{Line: []byte(`{"type":"result"}`), TurnEnded: true, Answering: []string{first.id}, SessionID: "session-1"})

	expectExit(t, proc)
	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	if !strings.Contains(describe(events), `{"prompt":`) || !strings.Contains(describe(events), `"outcome":"withdrawn"`) {
		t.Errorf("events: %s, want the prompt released as withdrawn", describe(events))
	}
	var detail struct {
		Queued []any `json:"queued"`
	}
	srv.call(t, http.MethodGet, "/v1/tasks/"+string(task), nil, &detail)
	if len(detail.Queued) != 0 {
		t.Errorf("queued = %+v, want none", detail.Queued)
	}
	if status, body := srv.try(t, http.MethodPost, "/v1/tasks/"+string(task)+"/commands",
		map[string]any{"kind": protocol.CommandWithdraw, "payload": protocol.Withdraw{Prompt: held}}); status != http.StatusConflict {
		t.Errorf("second withdrawal: %d %s, want 409", status, body)
	}
}

func TestGivenHeldPromptWhenTheTurnEndsWithThePauseSettledThenThePromptIsDroppedAndReportedSo(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task, proc, first := runningTurn(t, srv, d)
	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "Later."})
	heldPromptID(t, srv, task)
	srv.command(t, task, protocol.CommandPause, nil)
	pauseRequest := proc.nextInput(t)
	acknowledgePauseThroughGateway(t, proc, "Stopped.")

	proc.emit(harness.Output{Line: []byte(`{"type":"result"}`), TurnEnded: true, Answering: []string{first.id, pauseRequest.id}, SessionID: "session-1"})

	expectExit(t, proc)
	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	if !strings.Contains(describe(events), `"outcome":"dropped"`) {
		t.Errorf("events: %s, want the held prompt dropped", describe(events))
	}
	srv.waitForState(t, task, "paused")
}
