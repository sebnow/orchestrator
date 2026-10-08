package daemon

import (
	"errors"
	"slices"
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

// requireSettledAfter checks that the journal holds exactly one
// pause_settled, with payload want, and that it directly follows an event
// of kind after.
func requireSettledAfter(t *testing.T, events []protocol.Event, after protocol.Kind, want string) {
	t.Helper()
	var found []string
	for idx, event := range events {
		if event.Kind != protocol.KindPauseSettled {
			continue
		}
		found = append(found, string(event.Payload))
		if idx == 0 || events[idx-1].Kind != after {
			t.Errorf("pause_settled follows %v, want %s", kinds(events[:idx]), after)
		}
	}
	if len(found) != 1 || found[0] != want {
		t.Errorf("pause_settled payloads = %q, want one %s", found, want)
	}
}

func TestGivenTaskWhoseTurnHasEndedWhenPausingThenErrTaskEndedBecauseItsProcessIsExiting(t *testing.T) {
	gateway := startTestGateway(t)
	synctest.Test(t, func(t *testing.T) {
		f, promptID := startPauseTask(t, gateway, testLimits)
		endTurn(f, promptID)
		synctest.Wait()

		err := f.task.Pause()

		if !errors.Is(err, ErrTaskEnded) {
			t.Errorf("pause = %v, want ErrTaskEnded", err)
		}
		if in := f.proc.nextInput(t); in.kind != "close" {
			t.Errorf("input = %+v, want the input closed", in)
		}
		<-f.task.Done()
		if got := kinds(f.journal(t)); slices.Contains(got, protocol.KindPauseSettled) {
			t.Errorf("journal kinds = %v, want no pause_settled", got)
		}
	})
}

func TestGivenProcessNotYetPromptedWhenPausingThenItIsPausedAtOnceAndItsInputClosed(t *testing.T) {
	spec := testTaskSpec
	spec.Prompt = ""
	f := startTestTask(t, startTestGateway(t), spec)

	if err := f.task.Pause(); err != nil {
		t.Fatal(err)
	}

	if in := f.proc.nextInput(t); in.kind != "close" {
		t.Errorf("input = %+v, want the input closed", in)
	}
	<-f.task.Done()
	if s := f.task.State(); s.Pause != Paused || !s.Closing {
		t.Errorf("state = %+v", s)
	}
	requireSettledAfter(t, f.journal(t), protocol.KindHarnessStarted, `{"interrupted":false}`)
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
		if got := kinds(f.journal(t)); slices.Contains(got, protocol.KindPauseSettled) {
			t.Errorf("journal kinds = %v, want no pause_settled before the turn ends", got)
		}
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
		if in := f.proc.nextInput(t); in.kind != "close" {
			t.Errorf("input = %+v, want the input closed once the pause settled", in)
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
		requireSettledAfter(t, events, protocol.KindHarnessOutput, `{"interrupted":false}`)
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
		requireSettledAfter(t, f.journal(t), protocol.KindHarnessOutput, `{"interrupted":true}`)
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

func TestGivenSettledPauseWhenTheTurnEndsThenTheProcessExitsKeepingTheNoteAndAPromptWaitsForTheNextProcess(t *testing.T) {
	gateway := startTestGateway(t)
	synctest.Test(t, func(t *testing.T) {
		f, promptID := startPauseTask(t, gateway, testLimits)
		f.task.Pause()
		pauseID := f.proc.nextInput(t).id
		f.task.acknowledgePause("after step 1")
		endTurn(f, promptID, pauseID)
		synctest.Wait()

		err := f.task.Prompt("Resume the task from where you stopped and finish it.")

		if !errors.Is(err, ErrTaskEnded) {
			t.Errorf("prompt = %v, want ErrTaskEnded", err)
		}
		if in := f.proc.nextInput(t); in.kind != "close" {
			t.Errorf("input = %+v, want the input closed", in)
		}
		f.proc.noInput(t)
		<-f.task.Done()
		s := f.task.State()
		if s.Pause != Paused || s.StopNote != "after step 1" || s.Running || s.Exit.ExitCode != 0 {
			t.Errorf("state = %+v", s)
		}
		events := f.journal(t)
		if got := kinds(events[len(events)-2:]); !slices.Equal(got, []protocol.Kind{protocol.KindPauseSettled, protocol.KindHarnessExited}) {
			t.Errorf("journal ends %v, want pause_settled then harness_exited", got)
		}
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
