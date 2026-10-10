package daemon

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
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
	// sessionSeen, when set, hears each harness session a task's process
	// reports that differs from the one it resumed or last reported, so
	// that a restart finds it recorded. It runs on the goroutine reading
	// the harness's output.
	sessionSeen func(protocol.TaskID, string)
	// harnessStarted, when set, hears the pid of each harness process right
	// after it starts, before its start is journaled, so that a restart
	// can find the process if it outlives the daemon.
	harnessStarted func(protocol.TaskID, int)
	// processes finds and kills the harness processes a previous daemon
	// left running.
	processes processTable
	// workspaces holds each task's workspace, in a directory named after
	// the task.
	workspaces string
	// runner runs the harness and the commands that touch workspaces as
	// the harness user, or as the daemon's own user when it is zero.
	runner runner
	// harnessFiles is where the harness writes files its process reads,
	// as harness.Spec.FileDir; empty leaves the choice to the harness.
	harnessFiles string
}

// New returns a daemon that keeps journals under stateDir, runs h, and
// serves its agents on gateway. observe may be nil.
func New(stateDir string, h harness.Harness, gateway *Gateway, observe func(protocol.Event)) *Daemon {
	if observe == nil {
		observe = func(protocol.Event) {}
	}
	return &Daemon{stateDir: stateDir, harness: h, gateway: gateway, observe: observe, processes: psTable{}, workspaces: filepath.Join(stateDir, "workspaces")}
}

// workspace returns the directory task works in.
func (d *Daemon) workspace(task protocol.TaskID) string {
	return filepath.Join(d.workspaces, string(task))
}

// deleteWorkspace deletes the workspace of task once its work is
// delivered, as runner.deleteWorkspace does.
func (d *Daemon) deleteWorkspace(task protocol.TaskID) error {
	return d.runner.deleteWorkspace(d.workspace(task), task)
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
	// Tools are the gateway tools the agent may call besides the
	// permission and pause tools, as protocol.StartTask.Tools has them:
	// nil allows every one.
	Tools []string
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
	// CutShort is set once Running is false when Stop or Interrupt
	// interrupted the running turn and Exit reports it cut short rather than the
	// harness's own exit.
	CutShort bool
}

// Task is one running harness and its journal.
type Task struct {
	id      protocol.TaskID
	harness harness.Harness
	// workdir is where the harness runs; its work is delivered from there
	// when the process exits, by runner.
	workdir string
	runner  runner
	proc    harness.Process
	journal *journal
	observe func(protocol.Event)
	// sessionSeen is the daemon's, for this task; nil when it has none.
	sessionSeen func(string)
	done        chan struct{}

	// commands serialises the commands that write to the harness, so state
	// changes and stdin writes happen in the same order without holding mu
	// while a write blocks.
	commands sync.Mutex

	mu          sync.Mutex
	changed     chan struct{}
	outstanding map[string]bool
	// held are the prompts the daemon holds until the running turn ends,
	// oldest first; the first is sent then.
	held        []heldPrompt
	permissions map[string]*pendingPermission
	turnsEnded  int
	closing     bool
	session     string
	exit        *protocol.HarnessExited
	limits      PauseLimits
	pause       pause
	// cutShort is the one Stop or Interrupt was given when it interrupted
	// a running turn, until a prompt starts another; nil otherwise.
	cutShort     func(session string, own protocol.HarnessExited) string
	exitCutShort bool
}

type pendingPermission struct {
	request protocol.PermissionRequested
	answer  chan harness.Decision
}

// heldPrompt is a prompt held until the running turn ends: the id of the
// command that carried it, and its text.
type heldPrompt struct {
	ref  uint64
	text string
}

// outgoing is a held prompt on its way to the harness under id.
type outgoing struct {
	heldPrompt
	id string
}

// ErrNotHeld reports a withdrawal of a prompt the task does not hold:
// sent already, dropped, or never held.
var ErrNotHeld = errors.New("prompt is not held")

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
		workdir:     spec.Workdir,
		runner:      d.runner,
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
	if d.sessionSeen != nil {
		t.sessionSeen = func(session string) { d.sessionSeen(spec.ID, session) }
	}
	url, unregister, err := d.gateway.register(spec.ID, spec.Tools, gatewayTask{
		permission:       t.askPermission,
		acknowledgePause: t.acknowledgePause,
		spawnTask:        d.spawnTask(spec.ID),
		sendMessage:      d.sendMessage(spec.ID),
	})
	if err != nil {
		t.record(protocol.KindHarnessExited, protocol.HarnessExited{ExitCode: -1, Error: harnessNotStarted(err)})
		j.close()
		return nil, err
	}
	proc, err := d.harness.Start(ctx, harness.Spec{
		Workdir:      spec.Workdir,
		Model:        spec.Model,
		SystemPrompt: spec.SystemPrompt,
		Resume:       spec.Session,
		RunAs:        d.runner.as,
		FileDir:      d.harnessFiles,
		Gateway: harness.Gateway{
			URL:            url,
			PermissionTool: PermissionTool,
			Tools:          gatewayTools(spec.Tools),
		},
	})
	if err != nil {
		t.record(protocol.KindHarnessExited, protocol.HarnessExited{ExitCode: -1, Error: harnessNotStarted(err)})
		unregister()
		j.close()
		return nil, err
	}
	t.proc = proc
	if d.harnessStarted != nil {
		d.harnessStarted(spec.ID, proc.PID())
	}
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

// The error texts of the harness_exited the daemon reports for a process
// that never started, before its workspace was ready and after,
// followed by ": " and the cause. internal/server words them the same,
// and reports the failure as what it is rather than as an exit.
const (
	workspaceNotPrepared = "workspace could not be prepared"
	harnessCannotStart   = "harness could not be started"
)

// harnessNotStarted is the error text for a harness that could not be
// started because of cause.
func harnessNotStarted(cause error) string {
	return harnessCannotStart + ": " + cause.Error()
}

// gatewayTools are the gateway tools the harness lets the agent call
// without asking: the pause tool, and those of allowed, as
// TaskSpec.Tools has them.
func gatewayTools(allowed []string) []string {
	tools := []string{AcknowledgePauseTool}
	for _, tool := range []string{SpawnTaskTool, SendMessageTool} {
		if allowed == nil || slices.Contains(allowed, tool) {
			tools = append(tools, tool)
		}
	}
	return tools
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
	reported := t.State().SessionID
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
		if out.SessionID != "" && out.SessionID != reported {
			reported = out.SessionID
			if t.sessionSeen != nil {
				t.sessionSeen(reported)
			}
		}
		if out.Quota != nil {
			if _, err := t.record(protocol.KindQuotaObserved, *out.Quota); err != nil {
				t.proc.Kill()
				continue
			}
		}
		// The settlement is journaled before the next line is read, so it
		// precedes the harness's exit.
		settled, turnOver, next := t.handleOutput(out)
		if settled != nil {
			if _, err := t.record(protocol.KindPauseSettled, *settled); err != nil {
				t.proc.Kill()
			}
		}
		if next != nil {
			turnOver = t.sendHeld(*next)
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
	t.mu.Lock()
	for _, p := range t.held {
		t.record(protocol.KindPromptReleased, protocol.PromptReleased{Prompt: p.ref, Outcome: protocol.ReleasedDropped})
	}
	t.held = nil
	t.mu.Unlock()
	exit := t.proc.Wait()
	if readErr != nil && exit.Error == "" {
		exit.Error = "read harness output: " + readErr.Error()
	}
	// The work is pushed before the exit is journaled, so that the server
	// holds the push once it holds the end of the turn.
	delivery, cancel := context.WithTimeout(context.Background(), pushTimeout)
	if pushed := t.runner.deliver(delivery, t.workdir, t.id); pushed != nil {
		t.record(protocol.KindBranchPushed, *pushed)
	}
	cancel()
	exit, cutShort := t.reportCutShort(exit)
	t.record(protocol.KindHarnessExited, exit)
	unregister()
	t.journal.close()

	t.mu.Lock()
	t.exit = &exit
	t.exitCutShort = cutShort
	t.pause.stopTimer()
	t.notifyLocked()
	t.mu.Unlock()
	close(t.done)
}

// reportCutShort returns the exit to journal for the harness's own exit
// own, and whether it reports the turn cut short. A turn Stop or
// Interrupt interrupted is reported with the error its cutShort gives,
// and ExitCode -1, unless the harness exited cleanly or cutShort gives no
// error. Claude Code exits 1 after an interrupted turn, because the
// turn's result is an error (docs/design/2026-10-08-shutdown-findings.md,
// docs/design/2026-10-08-interrupt-findings.md).
func (t *Task) reportCutShort(own protocol.HarnessExited) (protocol.HarnessExited, bool) {
	t.mu.Lock()
	cutShort, session := t.cutShort, t.session
	t.mu.Unlock()
	if cutShort == nil || own.ExitCode == 0 && own.Error == "" {
		return own, false
	}
	text := cutShort(session, own)
	if text == "" {
		return own, false
	}
	return protocol.HarnessExited{ExitCode: -1, Error: text, Stderr: own.Stderr}, true
}

// handleOutput updates the task's state from one line of output. It
// returns the settlement to record when the line settles a pause, and
// whether the line ends the process's last turn: a turn ended and nothing
// the daemon sent waits for another (docs/adr/2026-10-08-task-lifetime.md).
// From then on the task takes no more commands, so nothing can be written
// to the harness after the daemon decides to close its input.
//
// A turn that ends with nothing the daemon sent waiting and a prompt held
// does not end the process: it returns the oldest held prompt as next,
// outstanding already, for sendHeld to send. A turn that settles a pause
// drops every held prompt instead, since the task is to stop.
func (t *Task) handleOutput(out harness.Output) (settled *protocol.PauseSettled, turnOver bool, next *outgoing) {
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
		return nil, false, nil
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
	if t.pause.state == Paused {
		for _, p := range t.held {
			t.record(protocol.KindPromptReleased, protocol.PromptReleased{Prompt: p.ref, Outcome: protocol.ReleasedDropped})
		}
		t.held = nil
	}
	switch {
	case len(t.outstanding) > 0 || t.closing:
	case len(t.held) > 0:
		next = &outgoing{heldPrompt: t.held[0], id: newID()}
		t.held = t.held[1:]
		t.outstanding[next.id] = true
		t.cutShort = nil
	default:
		t.closing = true
		turnOver = true
	}
	t.notifyLocked()
	return settled, turnOver, next
}

// sendHeld sends p, a held prompt handleOutput took for the next turn, to
// the harness, and reports whether the process's turn is over after all:
// p could not be sent and nothing else waits. A prompt the process can no
// longer take, because Stop closed its input meanwhile, is dropped.
func (t *Task) sendHeld(p outgoing) (turnOver bool) {
	t.commands.Lock()
	defer t.commands.Unlock()
	t.mu.Lock()
	ended := t.ended()
	t.mu.Unlock()
	var err error
	if !ended {
		err = t.proc.Prompt(p.id, p.text)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if ended || err != nil {
		delete(t.outstanding, p.id)
		t.record(protocol.KindPromptReleased, protocol.PromptReleased{Prompt: p.ref, Outcome: protocol.ReleasedDropped})
		if len(t.outstanding) == 0 && !t.closing {
			t.closing = true
			turnOver = true
		}
		t.notifyLocked()
		return turnOver
	}
	t.record(protocol.KindPromptReleased, protocol.PromptReleased{Prompt: p.ref, Outcome: protocol.ReleasedSent})
	return false
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
		CutShort:      t.exitCutShort,
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
	t.cutShort = nil
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

// Hold holds text, the prompt command ref carried, until the running turn
// ends, when it is sent as the next turn's prompt, and reports
// PromptHeld. Unlike a prompt sent at once, which the harness queues
// where it cannot be withdrawn, a held prompt can be withdrawn until then.
// While a pause is in progress it returns ErrBusy. Once the process's
// turn is over it returns ErrTaskEnded: the prompt belongs to the task's
// next process.
func (t *Task) Hold(ref uint64, text string) error {
	t.commands.Lock()
	defer t.commands.Unlock()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ended() {
		return ErrTaskEnded
	}
	if t.pause.unsettled() {
		return fmt.Errorf("%w: a pause is in progress", ErrBusy)
	}
	if _, err := t.record(protocol.KindPromptHeld, protocol.PromptHeld{Prompt: ref}); err != nil {
		return fmt.Errorf("record the held prompt: %w", err)
	}
	t.held = append(t.held, heldPrompt{ref: ref, text: text})
	t.notifyLocked()
	return nil
}

// Withdraw drops the held prompt that command ref carried, and reports
// PromptReleased. A prompt no longer held gets ErrNotHeld.
func (t *Task) Withdraw(ref uint64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	idx := slices.IndexFunc(t.held, func(p heldPrompt) bool { return p.ref == ref })
	if idx < 0 {
		return fmt.Errorf("%w: %d", ErrNotHeld, ref)
	}
	t.held = slices.Delete(t.held, idx, idx+1)
	t.record(protocol.KindPromptReleased, protocol.PromptReleased{Prompt: ref, Outcome: protocol.ReleasedWithdrawn})
	t.notifyLocked()
	return nil
}

// Steer interrupts the running turn and sends text as the next prompt in
// the same session. The interrupt is the owner's steering, not a turn cut
// short: the turn it starts ends as the harness ends it. Prompts held
// stay held, to follow the steering prompt. While a pause is in progress
// it returns ErrBusy. Once the process's turn is over it returns
// ErrTaskEnded: the prompt belongs to the task's next process.
func (t *Task) Steer(text string) error {
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
	t.cutShort = nil
	// The interrupt ends the turn the prompts outstanding started, whether
	// or not its result says it answered them. The steering prompt is
	// outstanding before the interrupt, so that the interrupted turn's
	// end does not end the process.
	clear(t.outstanding)
	t.outstanding[id] = true

	t.notifyLocked()
	t.mu.Unlock()

	if err := t.proc.Interrupt(); err != nil {
		t.mu.Lock()
		delete(t.outstanding, id)
		t.notifyLocked()
		t.mu.Unlock()
		return fmt.Errorf("interrupt: %w", err)
	}
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
//
// cutShort, when not nil, words the end of the turn as Stop's does: unless
// the harness exits cleanly, the task journals a harness_exited with
// ExitCode -1 and the error cutShort returns, and State.CutShort is set.
// A prompt sent after the interrupt starts a turn of its own, whose end is
// the harness's own again.
func (t *Task) Interrupt(cutShort func(session string, own protocol.HarnessExited) string) error {
	t.commands.Lock()
	defer t.commands.Unlock()
	// Set before the interrupt is sent, so that the harness cannot exit
	// before the task knows its turn was cut short.
	t.mu.Lock()
	if !t.ended() {
		t.cutShort = cutShort
	}
	t.mu.Unlock()
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
//
// cutShort, when not nil, words the end of a turn that Stop interrupts:
// unless the harness then exits cleanly, the task journals a
// harness_exited with ExitCode -1 and the error cutShort returns, given
// the session the task last reported and the harness's own exit, and
// State.CutShort is set. An empty error keeps the harness's own exit.
func (t *Task) Stop(ctx context.Context, cutShort func(session string, own protocol.HarnessExited) string) protocol.HarnessExited {
	t.commands.Lock()
	// Set before the interrupt is sent, so that the harness cannot exit
	// before the task knows its turn was cut short.
	t.mu.Lock()
	if !t.ended() {
		t.cutShort = cutShort
	}
	t.mu.Unlock()
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

// Kill kills the harness at once, without interrupting its turn first,
// and waits until its exit is journaled.
func (t *Task) Kill() {
	t.proc.Kill()
	<-t.done
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
