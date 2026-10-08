package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

var (
	// ErrTaskEnded reports a command for a task whose harness has exited.
	// The command can no longer apply and should be dropped.
	ErrTaskEnded = errors.New("task has ended")
	// ErrStale reports an answer to a permission request that is no longer
	// waiting: answered already, abandoned by the harness, or never made.
	ErrStale = errors.New("permission request is not pending")
	// ErrBusy reports a command that conflicts with a pause in progress.
	// It can be retried once the pause has settled.
	ErrBusy = errors.New("task is busy")
)

// Daemon runs tasks on this machine.
type Daemon struct {
	stateDir string
	harness  harness.Harness
	gateway  *Gateway
	// forward carries agents' requests to the server; nil answers them
	// with errNotConnected. Serve sets it before any task starts.
	forward forwarder
	// observe sees every event right after it is journaled. It runs on the
	// goroutine that produced the event, so it must not block for long.
	observe func(protocol.Event)
}

// New returns a daemon that keeps journals under stateDir, runs h, and
// serves its agents on gateway. observe may be nil.
func New(stateDir string, h harness.Harness, gateway *Gateway, observe func(protocol.Event)) *Daemon {
	if observe == nil {
		observe = func(protocol.Event) {}
	}
	return &Daemon{stateDir: stateDir, harness: h, gateway: gateway, observe: observe}
}

// TaskSpec is what the daemon needs to run a task.
type TaskSpec struct {
	ID protocol.TaskID
	// Prompt is sent once the harness has started; empty sends nothing.
	Prompt       string
	Workdir      string
	Model        string
	SystemPrompt string
	Pause        PauseLimits
	// Session, when set, is the harness session the process resumes.
	Session string
}

// State is a snapshot of a task.
type State struct {
	// Running is true until the harness process has exited.
	Running bool
	// Closing is true once the daemon has closed the harness's input
	// because its turn ended; the process takes no more commands.
	Closing bool
	// SessionID is the harness session the process reported, or the one
	// it resumed until it reports one.
	SessionID string
	// Busy is true while a prompt the daemon sent has not been answered by
	// the end of a turn.
	Busy bool
	// TurnsEnded counts the turns the harness has finished.
	TurnsEnded  int
	Permissions []protocol.PermissionRequested
	// Exit is set once Running is false.
	Exit  *protocol.HarnessExited
	Pause PauseState
	// PausePickedUp is set once the harness shows a turn answering the
	// pause request.
	PausePickedUp bool
	// StopNote is the agent's note from its latest pause acknowledgement.
	StopNote string
}

// Task is one running harness and its journal.
type Task struct {
	id      protocol.TaskID
	harness harness.Harness
	proc    harness.Process
	journal *journal
	observe func(protocol.Event)
	done    chan struct{}

	// commands serialises the commands that write to the harness, so state
	// changes and stdin writes happen in the same order without holding mu
	// while a write blocks.
	commands sync.Mutex

	mu          sync.Mutex
	changed     chan struct{}
	outstanding map[string]bool
	permissions map[string]*pendingPermission
	turnsEnded  int
	closing     bool
	session     string
	exit        *protocol.HarnessExited
	limits      PauseLimits
	pause       pause
}

type pendingPermission struct {
	request protocol.PermissionRequested
	answer  chan harness.Decision
}

// StartTask journals the task's start, starts its harness, and sends the
// task's prompt. The harness is killed if ctx is cancelled.
func (d *Daemon) StartTask(ctx context.Context, spec TaskSpec) (*Task, error) {
	if err := spec.Pause.validate(); err != nil {
		return nil, fmt.Errorf("task %s: %w", spec.ID, err)
	}
	j, err := createJournal(d.stateDir, spec.ID, d.harness.Info())
	if err != nil {
		return nil, err
	}
	return d.start(ctx, j, spec)
}

// start starts the harness of a task whose journal j holds no event yet.
// If the harness cannot be started the journal records why in
// harness_exited and is closed.
func (d *Daemon) start(ctx context.Context, j *journal, spec TaskSpec) (*Task, error) {
	t := &Task{
		id:          spec.ID,
		harness:     d.harness,
		journal:     j,
		observe:     d.observe,
		done:        make(chan struct{}),
		changed:     make(chan struct{}),
		outstanding: map[string]bool{},
		permissions: map[string]*pendingPermission{},
		limits:      spec.Pause,
		session:     spec.Session,
	}
	url, unregister, err := d.gateway.register(spec.ID, gatewayTask{
		permission:       t.askPermission,
		acknowledgePause: t.acknowledgePause,
		spawnTask:        d.spawnTask(spec.ID),
		sendMessage:      d.sendMessage(spec.ID),
	})
	if err != nil {
		t.record(protocol.KindHarnessExited, protocol.HarnessExited{ExitCode: -1, Error: err.Error()})
		j.close()
		return nil, err
	}
	proc, err := d.harness.Start(ctx, harness.Spec{
		Workdir:      spec.Workdir,
		Model:        spec.Model,
		SystemPrompt: spec.SystemPrompt,
		Resume:       spec.Session,
		Gateway: harness.Gateway{
			URL:            url,
			PermissionTool: PermissionTool,
			Tools:          []string{AcknowledgePauseTool, SpawnTaskTool, SendMessageTool},
		},
	})
	if err != nil {
		t.record(protocol.KindHarnessExited, protocol.HarnessExited{ExitCode: -1, Error: err.Error()})
		unregister()
		j.close()
		return nil, err
	}
	t.proc = proc
	if _, err := t.record(protocol.KindHarnessStarted, protocol.HarnessStarted{PID: proc.PID(), Model: spec.Model, Workdir: spec.Workdir}); err != nil {
		proc.Kill()
	}
	// The prompt is outstanding before any output is read, so that no
	// turn can end the process before it is answered.
	var promptErr error
	if spec.Prompt != "" {
		promptErr = t.Prompt(spec.Prompt)
	}
	go t.run(unregister)
	if promptErr != nil {
		return t, fmt.Errorf("send task prompt: %w", promptErr)
	}
	return t, nil
}

func (t *Task) ID() protocol.TaskID {
	return t.id
}

// record journals a control event and shows it to the observer.
func (t *Task) record(kind protocol.Kind, payload any) (protocol.Event, error) {
	event, err := t.journal.appendControl(kind, payload)
	if err == nil {
		t.observe(event)
	}
	return event, err
}

// run reads the harness's output until it exits. Each line is journaled
// before the task acts on it; if the journal fails the harness is killed,
// because nothing it does after that could be recorded.
func (t *Task) run(unregister func()) {
	var readErr error
	for {
		out, err := t.proc.Read()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}
		event, err := t.journal.appendOutput(out.Line)
		if err != nil {
			t.proc.Kill()
			continue
		}
		t.observe(event)
		if out.Quota != nil {
			if _, err := t.record(protocol.KindQuotaObserved, *out.Quota); err != nil {
				t.proc.Kill()
				continue
			}
		}
		// The settlement is journaled before the next line is read, so it
		// precedes the harness's exit.
		settled, turnOver := t.handleOutput(out)
		if settled != nil {
			if _, err := t.record(protocol.KindPauseSettled, *settled); err != nil {
				t.proc.Kill()
			}
		}
		if turnOver {
			// A failed close means the harness is gone; its exit ends the
			// task.
			t.proc.CloseInput()
		}
	}
	if readErr != nil {
		t.proc.Kill()
	}
	exit := t.proc.Wait()
	if readErr != nil && exit.Error == "" {
		exit.Error = "read harness output: " + readErr.Error()
	}
	t.record(protocol.KindHarnessExited, exit)
	unregister()
	t.journal.close()

	t.mu.Lock()
	t.exit = &exit
	t.pause.stopTimer()
	t.notifyLocked()
	t.mu.Unlock()
	close(t.done)
}

// handleOutput updates the task's state from one line of output. It
// returns the settlement to record when the line settles a pause, and
// whether the line ends the process's last turn: a turn ended and nothing
// the daemon sent waits for another (docs/adr/2026-10-08-task-lifetime.md).
// From then on the task takes no more commands, so nothing can be written
// to the harness after the daemon decides to close its input.
func (t *Task) handleOutput(out harness.Output) (settled *protocol.PauseSettled, turnOver bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if out.SessionID != "" {
		t.session = out.SessionID
	}
	if t.pause.unsettled() && !t.pause.pickedUp && slices.Contains(out.Answering, t.pause.id) {
		t.pause.pickedUp = true
		t.notifyLocked()
	}
	if !out.TurnEnded {
		return nil, false
	}
	t.turnsEnded++
	for _, id := range out.Answering {
		delete(t.outstanding, id)
	}
	// The pause has taken effect once the turn that answers it has ended,
	// whether the agent stopped by itself or was interrupted.
	if t.pause.unsettled() && !t.outstanding[t.pause.id] {
		settled = &protocol.PauseSettled{Interrupted: t.pause.state == PauseInterrupting}
		t.pause.state = Paused
		t.pause.stopTimer()
	}
	if len(t.outstanding) == 0 && !t.closing {
		t.closing = true
		turnOver = true
	}
	t.notifyLocked()
	return settled, turnOver
}

// ended reports whether the process takes no more commands: its turn is
// over or it has exited. t.mu must be held.
func (t *Task) ended() bool {
	return t.closing || t.exit != nil
}

// lastSeq returns the seq of the task's last journaled event.
func (t *Task) lastSeq() uint64 {
	return t.journal.lastSeq()
}

// notifyLocked wakes every WaitFor. t.mu must be held.
func (t *Task) notifyLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// State returns a snapshot of the task.
func (t *Task) State() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stateLocked()
}

func (t *Task) stateLocked() State {
	s := State{
		Running:       t.exit == nil,
		Closing:       t.closing,
		SessionID:     t.session,
		Busy:          len(t.outstanding) > 0,
		TurnsEnded:    t.turnsEnded,
		Exit:          t.exit,
		Pause:         t.pause.state,
		PausePickedUp: t.pause.pickedUp,
		StopNote:      t.pause.note,
	}
	for _, p := range t.permissions {
		s.Permissions = append(s.Permissions, p.request)
	}
	return s
}

// WaitFor blocks until done reports true for the task's state, and returns
// that state.
func (t *Task) WaitFor(ctx context.Context, done func(State) bool) (State, error) {
	for {
		t.mu.Lock()
		s := t.stateLocked()
		changed := t.changed
		t.mu.Unlock()
		if done(s) {
			return s, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return s, ctx.Err()
		}
	}
}

// Done is closed once the harness has exited and its exit is journaled.
func (t *Task) Done() <-chan struct{} {
	return t.done
}

// Prompt sends text to the harness as a user prompt while a turn runs or
// before the first. While a pause is in progress it returns ErrBusy. Once
// the process's turn is over it returns ErrTaskEnded: the prompt belongs
// to the task's next process.
func (t *Task) Prompt(text string) error {
	t.commands.Lock()
	defer t.commands.Unlock()
	id := newID()
	t.mu.Lock()
	if t.ended() {
		t.mu.Unlock()
		return ErrTaskEnded
	}
	if t.pause.unsettled() {
		t.mu.Unlock()
		return fmt.Errorf("%w: a pause is in progress", ErrBusy)
	}
	t.pause = pause{note: t.pause.note}
	t.outstanding[id] = true
	t.notifyLocked()
	t.mu.Unlock()

	if err := t.proc.Prompt(id, text); err != nil {
		t.mu.Lock()
		delete(t.outstanding, id)
		t.notifyLocked()
		t.mu.Unlock()
		return fmt.Errorf("send prompt: %w", err)
	}
	return nil
}

// Interrupt stops the harness's running turn at once. The session stays
// alive and takes further prompts.
func (t *Task) Interrupt() error {
	t.commands.Lock()
	defer t.commands.Unlock()
	return t.interrupt()
}

// interrupt sends the interrupt. t.commands must be held.
func (t *Task) interrupt() error {
	t.mu.Lock()
	ended := t.ended()
	t.mu.Unlock()
	if ended {
		return ErrTaskEnded
	}
	if err := t.proc.Interrupt(); err != nil {
		return fmt.Errorf("interrupt: %w", err)
	}
	return nil
}

// Stop ends the task: it interrupts the running turn, closes the
// harness's input and waits for it to exit. If ctx ends first the harness
// is killed.
func (t *Task) Stop(ctx context.Context) protocol.HarnessExited {
	t.commands.Lock()
	t.interrupt()
	t.mu.Lock()
	closed := t.closing
	t.closing = true
	t.notifyLocked()
	t.mu.Unlock()
	if !closed {
		t.proc.CloseInput()
	}
	t.commands.Unlock()
	select {
	case <-t.done:
	case <-ctx.Done():
		t.proc.Kill()
		<-t.done
	}
	return *t.State().Exit
}

// AnswerPermission answers the pending permission request requestID.
func (t *Task) AnswerPermission(requestID string, decision harness.Decision) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	p, ok := t.permissions[requestID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrStale, requestID)
	}
	delete(t.permissions, requestID)
	p.answer <- decision
	t.notifyLocked()
	return nil
}

// askPermission serves the gateway's permission tool. It journals the
// request, then waits for AnswerPermission, for the harness to abandon the
// call, or for the task to end.
func (t *Task) askPermission(ctx context.Context, arguments json.RawMessage) (string, error) {
	req, err := t.harness.ParsePermission(arguments)
	if err != nil {
		return "", err
	}
	p := &pendingPermission{
		request: protocol.PermissionRequested{RequestID: newID(), Tool: req.Tool, Input: req.Input},
		answer:  make(chan harness.Decision, 1),
	}
	if _, err := t.record(protocol.KindPermissionRequested, p.request); err != nil {
		return "", fmt.Errorf("permission request not recorded: %w", err)
	}
	t.mu.Lock()
	if t.exit != nil {
		t.mu.Unlock()
		return t.harness.EncodeDecision(req, harness.Decision{Message: "The task has ended."}), nil
	}
	t.permissions[p.request.RequestID] = p
	t.notifyLocked()
	t.mu.Unlock()

	select {
	case decision := <-p.answer:
		return t.harness.EncodeDecision(req, decision), nil
	case <-ctx.Done():
		t.dropPermission(p.request.RequestID)
		return "", ctx.Err()
	case <-t.done:
		t.dropPermission(p.request.RequestID)
		return t.harness.EncodeDecision(req, harness.Decision{Message: "The task has ended."}), nil
	}
}

func (t *Task) dropPermission(requestID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.permissions[requestID]; ok {
		delete(t.permissions, requestID)
		t.notifyLocked()
	}
}

// newID returns a random version 4 UUID. Prompt ids must be UUIDs: the
// SDK reference types the uuid of a user message as one.
func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
