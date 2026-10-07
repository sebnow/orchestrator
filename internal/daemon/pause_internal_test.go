package daemon

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

var testLimits = PauseLimits{Acknowledge: 10 * time.Second, Cleanup: 30 * time.Second}

// startPauseTask starts a task inside a synctest bubble with the given
// limits and returns it with the id of its first prompt. gateway must be
// started outside the bubble.
func startPauseTask(t *testing.T, gateway *Gateway, limits PauseLimits) (taskFixture, string) {
	t.Helper()
	spec := testTaskSpec
	spec.Pause = limits
	f := startTestTask(t, gateway, spec)
	prompt := f.proc.nextInput(t)
	return f, prompt.id
}

// endTurn emits the line that ends a turn answering ids.
func endTurn(f taskFixture, ids ...string) {
	f.proc.emit(harness.Output{Line: []byte(`{"type":"result"}`), TurnEnded: true, Answering: ids})
}

func TestGivenIdleTaskWhenPausingThenItIsPausedAtOnceAndTheHarnessIsNotTold(t *testing.T) {
	gateway := startTestGateway(t)
	synctest.Test(t, func(t *testing.T) {
		f, promptID := startPauseTask(t, gateway, testLimits)
		endTurn(f, promptID)
		synctest.Wait()

		if err := f.task.Pause(); err != nil {
			t.Fatal(err)
		}

		if s := f.task.State(); s.Pause != Paused {
			t.Errorf("pause = %q, want paused", s.Pause)
		}
		f.proc.noInput(t)
		f.end(t, protocol.HarnessExited{})
	})
}

func TestGivenBusyTaskWhenPausingThenTheHarnessGetsThePauseRequestWithItsOwnID(t *testing.T) {
	gateway := startTestGateway(t)
	synctest.Test(t, func(t *testing.T) {
		f, promptID := startPauseTask(t, gateway, testLimits)

		if err := f.task.Pause(); err != nil {
			t.Fatal(err)
		}

		in := f.proc.nextInput(t)
		if in.kind != "prompt" || in.text != pausePrompt || in.id == promptID || !uuidPattern.MatchString(in.id) {
			t.Errorf("input = %+v", in)
		}
		if s := f.task.State(); s.Pause != PauseRequested || s.PausePickedUp {
			t.Errorf("state = %+v", s)
		}
		f.end(t, protocol.HarnessExited{})
	})
}

func TestGivenPauseRequestWhenATurnEchoesItsIDThenThePickupIsRecorded(t *testing.T) {
	gateway := startTestGateway(t)
	synctest.Test(t, func(t *testing.T) {
		f, promptID := startPauseTask(t, gateway, testLimits)
		f.task.Pause()
		pauseID := f.proc.nextInput(t).id

		f.proc.emit(harness.Output{Line: []byte(`{"type":"assistant"}`), Answering: []string{promptID, pauseID}})
		synctest.Wait()

		if s := f.task.State(); !s.PausePickedUp || s.Pause != PauseRequested {
			t.Errorf("state = %+v", s)
		}
		f.end(t, protocol.HarnessExited{})
	})
}

func TestGivenAcknowledgedPauseWhenTheTurnEndsThenTheTaskIsPausedWithTheNoteAndNeverInterrupted(t *testing.T) {
	gateway := startTestGateway(t)
	synctest.Test(t, func(t *testing.T) {
		f, promptID := startPauseTask(t, gateway, testLimits)
		f.task.Pause()
		pauseID := f.proc.nextInput(t).id
		time.Sleep(5 * time.Second)

		reply, err := f.task.acknowledgePause("Stopped after step 1; steps 2 and 3 remain.")
		if err != nil || reply != pauseConfirmation {
			t.Fatalf("acknowledge = %q, %v", reply, err)
		}
		if s := f.task.State(); s.Pause != PauseAcknowledged {
			t.Errorf("after acknowledgement pause = %q", s.Pause)
		}
		endTurn(f, promptID, pauseID)
		synctest.Wait()
		time.Sleep(time.Hour)

		s := f.task.State()
		if s.Pause != Paused || s.StopNote != "Stopped after step 1; steps 2 and 3 remain." || s.Busy {
			t.Errorf("state = %+v", s)
		}
		f.proc.noInput(t)
		f.end(t, protocol.HarnessExited{})
		events := f.journal(t)
		var notes []string
		for _, event := range events {
			if event.Kind == protocol.KindPauseAcknowledged {
				notes = append(notes, string(event.Payload))
			}
		}
		if len(notes) != 1 || notes[0] != `{"note":"Stopped after step 1; steps 2 and 3 remain."}` {
			t.Errorf("journaled acknowledgements = %q", notes)
		}
	})
}

func TestGivenNoAcknowledgementWhenTheFirstLimitPassesThenTheHarnessIsInterrupted(t *testing.T) {
	gateway := startTestGateway(t)
	synctest.Test(t, func(t *testing.T) {
		f, promptID := startPauseTask(t, gateway, testLimits)
		f.task.Pause()
		pauseID := f.proc.nextInput(t).id

		time.Sleep(testLimits.Acknowledge - time.Millisecond)
		synctest.Wait()
		f.proc.noInput(t)
		time.Sleep(time.Millisecond)
		synctest.Wait()

		if in := f.proc.nextInput(t); in.kind != "interrupt" {
			t.Fatalf("input = %+v, want interrupt", in)
		}
		if s := f.task.State(); s.Pause != PauseInterrupting {
			t.Errorf("pause = %q, want interrupting", s.Pause)
		}
		endTurn(f, promptID, pauseID)
		synctest.Wait()
		if s := f.task.State(); s.Pause != Paused {
			t.Errorf("after the interrupted turn pause = %q, want paused", s.Pause)
		}
		f.end(t, protocol.HarnessExited{})
	})
}

func TestGivenAcknowledgementWhenTheCleanupAllowanceRunsOutThenTheHarnessIsInterrupted(t *testing.T) {
	gateway := startTestGateway(t)
	synctest.Test(t, func(t *testing.T) {
		f, _ := startPauseTask(t, gateway, testLimits)
		f.task.Pause()
		f.proc.nextInput(t)
		time.Sleep(9 * time.Second)
		f.task.acknowledgePause("cleaning up")

		time.Sleep(testLimits.Cleanup - time.Millisecond)
		synctest.Wait()
		f.proc.noInput(t)
		time.Sleep(time.Millisecond)
		synctest.Wait()

		if in := f.proc.nextInput(t); in.kind != "interrupt" {
			t.Fatalf("input = %+v, want interrupt", in)
		}
		if s := f.task.State(); s.Pause != PauseInterrupting || s.StopNote != "cleaning up" {
			t.Errorf("state = %+v", s)
		}
		f.end(t, protocol.HarnessExited{})
	})
}

func TestGivenOneMillisecondLimitWhenPausingThenTheInterruptFollowsThePauseRequest(t *testing.T) {
	gateway := startTestGateway(t)
	synctest.Test(t, func(t *testing.T) {
		f, _ := startPauseTask(t, gateway, PauseLimits{Acknowledge: time.Millisecond, Cleanup: time.Millisecond})

		f.task.Pause()
		time.Sleep(time.Millisecond)
		synctest.Wait()

		first, second := f.proc.nextInput(t), f.proc.nextInput(t)
		if first.kind != "prompt" || first.text != pausePrompt || second.kind != "interrupt" {
			t.Errorf("inputs = %+v then %+v", first, second)
		}
		f.end(t, protocol.HarnessExited{})
	})
}

func TestGivenPauseInProgressWhenPromptingOrPausingAgainThenErrBusy(t *testing.T) {
	gateway := startTestGateway(t)
	synctest.Test(t, func(t *testing.T) {
		f, _ := startPauseTask(t, gateway, testLimits)
		f.task.Pause()
		f.proc.nextInput(t)

		if err := f.task.Prompt("Resume."); !errors.Is(err, ErrBusy) {
			t.Errorf("prompt = %v, want ErrBusy", err)
		}
		if err := f.task.Pause(); !errors.Is(err, ErrBusy) {
			t.Errorf("pause = %v, want ErrBusy", err)
		}
		f.proc.noInput(t)
		f.end(t, protocol.HarnessExited{})
	})
}

func TestGivenPausedTaskWhenPromptingThenTheTaskResumesAndKeepsTheStopNote(t *testing.T) {
	gateway := startTestGateway(t)
	synctest.Test(t, func(t *testing.T) {
		f, promptID := startPauseTask(t, gateway, testLimits)
		f.task.Pause()
		pauseID := f.proc.nextInput(t).id
		f.task.acknowledgePause("after step 1")
		endTurn(f, promptID, pauseID)
		synctest.Wait()

		if err := f.task.Prompt("Resume the task from where you stopped and finish it."); err != nil {
			t.Fatal(err)
		}

		in := f.proc.nextInput(t)
		if in.kind != "prompt" || in.text != "Resume the task from where you stopped and finish it." {
			t.Errorf("input = %+v", in)
		}
		if s := f.task.State(); s.Pause != NotPaused || s.PausePickedUp || s.StopNote != "after step 1" || !s.Busy {
			t.Errorf("state = %+v", s)
		}
		f.end(t, protocol.HarnessExited{})
	})
}

func TestGivenPauseInProgressWhenTheHarnessExitsThenNoInterruptFollows(t *testing.T) {
	gateway := startTestGateway(t)
	synctest.Test(t, func(t *testing.T) {
		f, _ := startPauseTask(t, gateway, testLimits)
		f.task.Pause()
		f.proc.nextInput(t)

		f.end(t, protocol.HarnessExited{ExitCode: 1})
		time.Sleep(time.Hour)
		synctest.Wait()

		f.proc.noInput(t)
		if err := f.task.Pause(); !errors.Is(err, ErrTaskEnded) {
			t.Errorf("pause after exit = %v, want ErrTaskEnded", err)
		}
	})
}

func TestGivenAcknowledgementWithoutAPauseWhenItArrivesThenItIsJournaledAndNothingChanges(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)

	reply, err := f.task.acknowledgePause("unprompted")

	if err != nil || reply != pauseConfirmation {
		t.Errorf("acknowledge = %q, %v", reply, err)
	}
	if s := f.task.State(); s.Pause != NotPaused || s.StopNote != "" {
		t.Errorf("state = %+v", s)
	}
	f.end(t, protocol.HarnessExited{})
	events := f.journal(t)
	if events[1].Kind != protocol.KindPauseAcknowledged {
		t.Errorf("kinds = %v", kinds(events))
	}
}

func TestGivenNonPositivePauseLimitsWhenStartingTaskThenItIsRefusedBeforeAnyJournal(t *testing.T) {
	h := newFakeHarness()
	d := New(t.TempDir(), h, startTestGateway(t), nil)
	spec := testTaskSpec
	spec.Pause = PauseLimits{Acknowledge: time.Second}

	if _, err := d.StartTask(t.Context(), spec); err == nil {
		t.Fatal("no error")
	}
	task, err := d.StartTask(t.Context(), testTaskSpec)
	if err != nil {
		t.Fatalf("the refused start left a journal behind: %v", err)
	}
	(<-h.started).end(protocol.HarnessExited{})
	<-task.Done()
}
