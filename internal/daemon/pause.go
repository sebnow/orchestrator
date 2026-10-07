package daemon

import (
	"fmt"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// PauseLimits bound a cooperative pause
// (docs/adr/2026-10-07-graceful-pause.md). The daemon interrupts the
// harness if the agent has not acknowledged the pause within Acknowledge
// of its delivery, or has not finished within Cleanup of acknowledging it.
type PauseLimits struct {
	Acknowledge time.Duration
	Cleanup     time.Duration
}

func (l PauseLimits) validate() error {
	if l.Acknowledge <= 0 || l.Cleanup <= 0 {
		return fmt.Errorf("pause limits must be positive, got %+v", l)
	}
	return nil
}

// PauseState is where a task is in a pause.
type PauseState string

const (
	NotPaused PauseState = ""
	// PauseRequested: the pause request was delivered; no acknowledgement
	// yet.
	PauseRequested PauseState = "requested"
	// PauseAcknowledged: the agent acknowledged and is finishing its step.
	PauseAcknowledged PauseState = "acknowledged"
	// PauseInterrupting: a limit passed and the daemon interrupted the
	// harness; the interrupted turn has not ended yet.
	PauseInterrupting PauseState = "interrupting"
	// Paused: the turn that answered the pause request has ended, or no
	// turn was running. A prompt resumes the task.
	Paused PauseState = "paused"
)

// pausePrompt asks the agent to pause. It is a stdin user message with no
// priority, so it reaches the model once the running tool call returns.
const pausePrompt = "Pause request from the operator. First call the " + AcknowledgePauseTool +
	" tool with a one-sentence note saying where you are stopping and what remains. " +
	"Then finish the step you are on, do not start another step, and end your turn."

// pauseConfirmation is the acknowledge_pause tool's reply to the agent.
const pauseConfirmation = "Pause acknowledged. Finish the step you are on, do not start another, then end your turn."

type pause struct {
	state    PauseState
	id       string
	pickedUp bool
	note     string
	timer    *time.Timer
}

func (p *pause) unsettled() bool {
	return p.state == PauseRequested || p.state == PauseAcknowledged || p.state == PauseInterrupting
}

func (p *pause) stopTimer() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
}

// Pause asks the agent to finish its current step and stop. A task with no
// turn running is paused at once and the harness is not told. The pause
// settles in the Paused state; Prompt resumes the task.
func (t *Task) Pause() error {
	t.commands.Lock()
	defer t.commands.Unlock()
	t.mu.Lock()
	if t.exit != nil {
		t.mu.Unlock()
		return ErrTaskEnded
	}
	if t.pause.unsettled() {
		t.mu.Unlock()
		return fmt.Errorf("%w: a pause is in progress", ErrBusy)
	}
	if len(t.outstanding) == 0 {
		t.pause = pause{state: Paused}
		t.notifyLocked()
		t.mu.Unlock()
		// t.commands is still held, so no prompt can start a turn before
		// the settlement is journaled.
		if _, err := t.record(protocol.KindPauseSettled, protocol.PauseSettled{}); err != nil {
			t.proc.Kill()
			return fmt.Errorf("pause settlement not recorded: %w", err)
		}
		return nil
	}
	id := newID()
	t.outstanding[id] = true
	t.pause = pause{state: PauseRequested, id: id}
	// The timer starts before the write; its interrupt still follows the
	// pause request on stdin because both hold t.commands.
	t.pause.timer = time.AfterFunc(t.limits.Acknowledge, func() { t.pauseDeadline(id) })
	t.notifyLocked()
	t.mu.Unlock()

	if err := t.proc.Prompt(id, pausePrompt); err != nil {
		t.mu.Lock()
		delete(t.outstanding, id)
		t.pause.stopTimer()
		t.pause = pause{}
		t.notifyLocked()
		t.mu.Unlock()
		return fmt.Errorf("send pause request: %w", err)
	}
	return nil
}

// pauseDeadline interrupts the harness when a pause limit passes before
// the pause has settled.
func (t *Task) pauseDeadline(id string) {
	t.commands.Lock()
	defer t.commands.Unlock()
	t.mu.Lock()
	if t.exit != nil || t.pause.id != id || (t.pause.state != PauseRequested && t.pause.state != PauseAcknowledged) {
		t.mu.Unlock()
		return
	}
	t.pause.state = PauseInterrupting
	t.pause.timer = nil
	t.notifyLocked()
	t.mu.Unlock()
	// A failed write means the harness is gone; its exit ends the task.
	t.proc.Interrupt()
}

// acknowledgePause serves the gateway's acknowledge_pause tool. The first
// acknowledgement of a pause replaces the acknowledgement limit with the
// cleanup allowance. The note is kept whenever a pause is under way or has
// settled; the acknowledgement is journaled in every case.
func (t *Task) acknowledgePause(note string) (string, error) {
	if _, err := t.record(protocol.KindPauseAcknowledged, protocol.PauseAcknowledged{Note: note}); err != nil {
		return "", fmt.Errorf("acknowledgement not recorded: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.pause.state == NotPaused {
		return pauseConfirmation, nil
	}
	t.pause.note = note
	if t.pause.state == PauseRequested && t.exit == nil {
		id := t.pause.id
		t.pause.state = PauseAcknowledged
		t.pause.stopTimer()
		t.pause.timer = time.AfterFunc(t.limits.Cleanup, func() { t.pauseDeadline(id) })
	}
	t.notifyLocked()
	return pauseConfirmation, nil
}
