package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

const (
	defaultMinBackoff      = 500 * time.Millisecond
	defaultMaxBackoff      = 30 * time.Second
	defaultShutdownTimeout = 30 * time.Second
)

// Config is what Serve needs to connect a daemon to the server.
type Config struct {
	// Server is the base URL of the server's HTTP API.
	Server *url.URL
	ID     protocol.DaemonID
	// StateDir holds the daemon's state, the task journals and the task
	// workspaces.
	StateDir string
	Harness  harness.Harness
	Gateway  *Gateway
	Log      *slog.Logger
	// Client makes the requests to the server; nil uses a new client.
	Client *http.Client
	// MinBackoff and MaxBackoff bound the wait between attempts to reach
	// the server; zero means 500 ms and 30 s.
	MinBackoff, MaxBackoff time.Duration
	// ShutdownTimeout bounds stopping the running tasks once ctx ends, and
	// then bounds sending their last events; zero means 30 s.
	ShutdownTimeout time.Duration

	// processes finds and kills harness processes; nil uses ps and
	// signals. Tests set it so that no real process is touched.
	processes processTable
}

// workspacePath returns the directory task works in under stateDir.
func workspacePath(stateDir string, task protocol.TaskID) string {
	return filepath.Join(stateDir, "workspaces", string(task))
}

// service is a daemon connected to the server: it applies the commands
// the server sends and sends the server every task's events.
type service struct {
	cfg    Config
	daemon *Daemon
	state  *state
	sender *sender
	log    *slog.Logger

	// stopping ends when the daemon begins to shut down. Workspace
	// preparation and waits inside commands end with it.
	stopping context.Context
	workers  sync.WaitGroup

	// mu guards running and every worker's queue.
	mu      sync.Mutex
	running map[protocol.TaskID]*worker
	// applied is the id of the last command applied. It leads the state's
	// copy when saving that failed.
	applied uint64
}

// worker applies one task's commands in order, on its own goroutine, so
// that a slow command such as a clone or a wait for a pause to settle
// holds up only its task.
type worker struct {
	queue []protocol.Command
	wake  chan struct{}
}

// Serve connects the daemon to the server and serves until ctx ends. It
// first ends the tasks a previous daemon process left behind. On return
// every task it started has been stopped, and their last events sent
// unless the server could not be reached within cfg.ShutdownTimeout.
func Serve(ctx context.Context, cfg Config) error {
	if cfg.Server == nil || cfg.Harness == nil || cfg.Gateway == nil || cfg.Log == nil {
		return errors.New("serve: Server, Harness, Gateway and Log are required")
	}
	if _, err := protocol.ParseDaemonID(string(cfg.ID)); err != nil {
		return err
	}
	stateDir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("state directory: %w", err)
	}
	cfg.StateDir = stateDir
	if cfg.Client == nil {
		cfg.Client = &http.Client{}
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = defaultMinBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = max(defaultMaxBackoff, cfg.MinBackoff)
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}

	st, err := loadState(stateDir)
	if err != nil {
		return err
	}
	// No task runs before snd is set: recovery appends to journals
	// without observing, and the sender learns those tasks from the state.
	var snd *sender
	d := New(stateDir, cfg.Harness, cfg.Gateway, func(event protocol.Event) { snd.notify(event.TaskID) })
	d.forward = forwardTo(cfg.Client, cfg.Server, cfg.ID)
	d.sessionSeen = func(task protocol.TaskID, session string) {
		if err := st.updateTask(task, func(rec *taskRecord) { rec.Session = session }); err != nil {
			cfg.Log.Error("record harness session", "task", task, "error", err)
		}
	}
	if cfg.processes != nil {
		d.processes = cfg.processes
	}
	d.harnessStarted = func(task protocol.TaskID, pid int) {
		started, err := d.processes.started(pid)
		if err != nil {
			cfg.Log.Warn("look up the harness's start time; a restart will leave it alone if it outlives the daemon", "task", task, "pid", pid, "error", err)
		}
		if err := st.updateTask(task, func(rec *taskRecord) { rec.Harness = &harnessProcess{PID: pid, Started: started} }); err != nil {
			cfg.Log.Error("record the harness process", "task", task, "error", err)
		}
	}
	if err := d.recoverTasks(st, cfg.Log, cfg.ShutdownTimeout); err != nil {
		return err
	}
	snd = newSender(cfg.Client, cfg.Server, cfg.ID, stateDir, st, cfg.Log, backoff{min: cfg.MinBackoff, max: cfg.MaxBackoff})

	stopping, stop := context.WithCancel(context.Background())
	defer stop()
	s := &service{
		cfg:      cfg,
		daemon:   d,
		state:    st,
		sender:   snd,
		log:      cfg.Log,
		stopping: stopping,
		running:  map[protocol.TaskID]*worker{},
		applied:  st.lastCommand(),
	}
	snd.moved = s.dropMoved

	sendCtx, cancelSend := context.WithCancel(context.Background())
	defer cancelSend()
	drain := make(chan struct{})
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		snd.run(sendCtx, drain)
	}()
	received := make(chan struct{})
	go func() {
		defer close(received)
		s.receive(ctx)
	}()
	cfg.Log.Info("serving", "server", cfg.Server.String(), "daemon", cfg.ID, "state_dir", stateDir)

	<-ctx.Done()
	<-received
	cfg.Log.Info("shutting down: stopping tasks")
	stop()
	s.workers.Wait()
	close(drain)
	select {
	case <-sent:
	case <-time.After(cfg.ShutdownTimeout):
		cfg.Log.Warn("shutting down: events not yet sent stay in the journals for the next start")
		cancelSend()
		<-sent
	}
	return nil
}

// accept registers a started task and starts its worker. The task is
// saved as known together with the command id before anything else
// happens, so that a restart neither runs the command again nor forgets
// the task.
func (s *service) accept(command protocol.Command) error {
	task := command.TaskID
	if _, err := os.Stat(JournalPath(s.cfg.StateDir, task)); s.state.known(task) || err == nil {
		s.log.Warn("start_task for a task this daemon has already run; skipped", "command", command.ID, "task", task)
		s.recordApplied(command.ID)
		return nil
	}
	if err := s.state.recordStart(command.ID, task); err != nil {
		return err
	}
	s.mu.Lock()
	s.spawnLocked(task, command)
	s.applied = command.ID
	s.mu.Unlock()
	return nil
}

// route queues a command for its task's worker. A task the daemon knows
// but has no worker for is between processes; a worker is started for
// it. A command for a task the daemon does not know is logged and
// skipped.
func (s *service) route(command protocol.Command) {
	s.mu.Lock()
	w, ok := s.running[command.TaskID]
	known := ok || s.state.known(command.TaskID)
	switch {
	case ok:
		w.queue = append(w.queue, command)
		select {
		case w.wake <- struct{}{}:
		default:
		}
	case known:
		s.spawnLocked(command.TaskID, command)
	}
	s.mu.Unlock()
	if !known {
		s.log.Warn("command for a task this daemon does not know; skipped",
			"command", command.ID, "kind", command.Kind, "task", command.TaskID)
	}
	s.recordApplied(command.ID)
}

// spawnLocked starts a worker for task with command queued. s.mu must be
// held.
func (s *service) spawnLocked(task protocol.TaskID, command protocol.Command) {
	w := &worker{queue: []protocol.Command{command}, wake: make(chan struct{}, 1)}
	s.running[task] = w
	s.workers.Go(func() { s.work(task, w) })
}

func (s *service) recordApplied(id uint64) {
	s.mu.Lock()
	s.applied = id
	s.mu.Unlock()
	// A command id that is not saved is only sent again after a restart,
	// when no task it could apply to is running any more.
	if err := s.state.recordCommand(id); err != nil {
		s.log.Error("record applied command", "command", id, "error", err)
	}
}

func (s *service) lastApplied() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applied
}

// work applies task's commands in order while the task has a process or
// commands are queued, starting a process for each command that needs
// one. When the daemon shuts down it stops the running process. The
// worker ends once the task has no process and no command waits.
func (s *service) work(task protocol.TaskID, w *worker) {
	var t *Task
	for {
		s.mu.Lock()
		commands := w.queue
		w.queue = nil
		if len(commands) == 0 && t == nil {
			delete(s.running, task)
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		for _, command := range commands {
			t = s.apply(task, t, command)
		}
		if t == nil {
			continue
		}
		select {
		case <-w.wake:
		case <-t.Done():
			s.processEnded(task, t, false)
			t = nil
		case <-s.stopping.Done():
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
			t.Stop(ctx, s.cutShortByShutdown(task))
			cancel()
			s.processEnded(task, t, false)
			t = nil
		}
	}
}

// awaitExit waits for the process t, whose turn is over, to exit, and
// records its end. If the daemon shuts down first, or the process has not
// exited within the shutdown timeout, it is stopped, and killed when it
// does not exit then either.
func (s *service) awaitExit(task protocol.TaskID, t *Task) {
	timeout := time.NewTimer(s.cfg.ShutdownTimeout)
	defer timeout.Stop()
	select {
	case <-t.Done():
	case <-s.stopping.Done():
		s.stopProcess(t)
	case <-timeout.C:
		s.log.Warn("process did not exit after its turn; stopping it", "task", task)
		s.stopProcess(t)
	}
	s.processEnded(task, t, false)
}

// stopProcess stops the process t, whose turn is over, so nothing it
// does is cut short.
func (s *service) stopProcess(t *Task) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	t.Stop(ctx, nil)
}

// cutShortByShutdown words the end of task's turn when the daemon's
// shutdown interrupts it: the task stays resumable, as after a restart
// (docs/adr/2026-10-08-shutdown-recovery.md), when its record allows.
func (s *service) cutShortByShutdown(task protocol.TaskID) func(string, protocol.HarnessExited) string {
	return func(session string, own protocol.HarnessExited) string {
		rec, _ := s.state.record(task)
		text := stopText(rec, session)
		if text != "" {
			s.log.Info("shutdown cut the turn short; the task can be resumed", "task", task, "exit_code", own.ExitCode, "error", own.Error)
		}
		return text
	}
}

// cutShortByInterrupt words the end of task's turn when the owner's
// interrupt cuts it short: the task stays resumable, as after a shutdown,
// when its record allows.
func (s *service) cutShortByInterrupt(task protocol.TaskID) func(string, protocol.HarnessExited) string {
	return func(session string, own protocol.HarnessExited) string {
		rec, _ := s.state.record(task)
		text := interruptText(rec, session)
		if text != "" {
			s.log.Info("the owner's interrupt cut the turn short; the task can be resumed", "task", task, "exit_code", own.ExitCode, "error", own.Error)
		}
		return text
	}
}

// processEnded records the end of task's process t: the harness session
// and the seq to resume from, whether it was paused, and whether the task
// has ended for good, because it was stopped or the harness failed
// without the daemon cutting its turn short. The journal is released to
// the sender. A task that has ended for good has its workspace
// deleted.
func (s *service) processEnded(task protocol.TaskID, t *Task, stopped bool) {
	st := t.State()
	clean := st.Exit != nil && st.Exit.ExitCode == 0 && st.Exit.Error == ""
	err := s.state.closeJournal(task, func(rec *taskRecord) {
		rec.Seq = t.lastSeq()
		if st.SessionID != "" {
			rec.Session = st.SessionID
		}
		rec.Paused = st.Pause == Paused
		rec.StopNote = ""
		if rec.Paused {
			rec.StopNote = st.StopNote
		}
		rec.Ended = rec.Ended || stopped || !clean && !st.CutShort
		rec.CutShort = st.CutShort
		rec.Interrupted = st.CutShort && (st.Exit.Error == interruptError || st.Exit.Error == interruptNoSessionError)
		rec.Harness = nil
	})
	if err != nil {
		s.log.Error("record the end of a process", "task", task, "error", err)
	}
	if rec, _ := s.state.record(task); rec.Ended {
		s.discardWorkspace(task)
	}
	s.sender.notify(task)
	s.log.Info("process ended", "task", task, "exit_code", st.Exit.ExitCode, "paused", st.Pause == Paused, "cut_short", st.CutShort, "stopped", stopped)
}

// startTask prepares the task's workspace and starts its first process.
// When either fails the task's journal ends with harness_exited saying
// why, the task has ended for good, and startTask returns nil.
func (s *service) startTask(command protocol.Command) *Task {
	task := command.TaskID
	j, err := s.state.openJournal(task, func(taskRecord) (*journal, error) {
		return createJournal(s.cfg.StateDir, task, s.cfg.Harness.Info())
	})
	if err != nil {
		// Without a journal the server cannot be told; a restart ends the
		// task, which the state knows.
		s.log.Error("start task", "task", task, "error", err)
		return nil
	}
	start, err := decodePayload[protocol.StartTask](command)
	if err != nil {
		return s.failStart(task, j, err)
	}
	limits := PauseLimits(start.PauseLimits)
	if err := limits.validate(); err != nil {
		return s.failStart(task, j, err)
	}
	workdir := workspacePath(s.cfg.StateDir, task)
	if err := prepareWorkspace(s.stopping, workdir, start.Workspace); err != nil {
		return s.failStart(task, j, fmt.Errorf("prepare workspace: %w", err))
	}
	// The settings make the task resumable, so they are recorded once its
	// workspace is ready.
	settings := taskSettings{Prompt: start.Prompt, Model: start.Model, SystemPrompt: start.SystemPrompt, Acknowledge: limits.Acknowledge, Cleanup: limits.Cleanup}
	if err := s.state.updateTask(task, func(rec *taskRecord) { rec.Settings = &settings }); err != nil {
		return s.failStart(task, j, fmt.Errorf("record task settings: %w", err))
	}
	return s.startProcess(task, j, TaskSpec{
		ID:           task,
		Prompt:       start.Prompt,
		Workdir:      workdir,
		Model:        start.Model,
		SystemPrompt: start.SystemPrompt,
		Pause:        limits,
	})
}

// resume starts a new process of task that continues its harness session
// (docs/adr/2026-10-08-task-lifetime.md) with followUp, or, when that is
// empty, with the daemon's words for resuming the task. A task with no
// recorded session starts a new one with its first prompt, followed by
// followUp (docs/adr/2026-10-08-restart-recovery.md). A task that cannot
// be resumed gets a harness_exited saying why, which ends it for good.
func (s *service) resume(task protocol.TaskID, followUp string) (*Task, error) {
	if s.stopping.Err() != nil {
		return nil, errors.New("the daemon is shutting down")
	}
	rec, ok := s.state.record(task)
	if !ok || rec.Ended {
		return nil, errors.New("the task has ended")
	}
	j, err := s.openTaskJournal(task)
	if err != nil {
		return nil, err
	}
	if !rec.resumable() {
		s.failStart(task, j, errors.New("no harness session to resume"))
		return nil, errors.New("no harness session to resume")
	}
	prompt := followUp
	switch {
	case rec.Session == "":
		s.log.Info("no harness session recorded; starting a new one with the task's first prompt", "task", task)
		prompt = rec.Settings.Prompt
		if followUp != "" {
			prompt += "\n\n" + followUp
		}
	case followUp == "":
		prompt = resumeText(rec)
	}
	t := s.startProcess(task, j, TaskSpec{
		ID:           task,
		Prompt:       prompt,
		Workdir:      workspacePath(s.cfg.StateDir, task),
		Model:        rec.Settings.Model,
		SystemPrompt: rec.Settings.SystemPrompt,
		Pause:        rec.Settings.limits(),
		Session:      rec.Session,
	})
	if t == nil {
		return nil, errors.New("the harness did not start")
	}
	return t, nil
}

// startProcess starts a process of task on its open journal j. When the
// harness does not start the journal ends with harness_exited saying why,
// the task has ended for good, and startProcess returns nil.
func (s *service) startProcess(task protocol.TaskID, j *journal, spec TaskSpec) *Task {
	// The harness outlives the daemon's context; shutting down stops it
	// gracefully.
	t, err := s.daemon.start(context.Background(), j, spec)
	if t == nil {
		s.log.Warn("task did not start", "task", task, "error", err)
		s.journalEnded(task, j)
		return nil
	}
	if err != nil {
		s.log.Warn("task prompt not sent", "task", task, "error", err)
	}
	s.log.Info("process started", "task", task, "workdir", spec.Workdir, "resumed", spec.Session != "")
	return t
}

// failStart ends the journal j of task, whose process did not start,
// with harness_exited saying why.
func (s *service) failStart(task protocol.TaskID, j *journal, cause error) *Task {
	s.log.Warn("task did not start", "task", task, "error", cause)
	j.appendControl(protocol.KindHarnessExited, protocol.HarnessExited{ExitCode: -1, Error: cause.Error()})
	j.close()
	s.journalEnded(task, j)
	return nil
}

// journalEnded records that task ended for good without a process
// running, releases its closed journal j, and deletes its workspace.
func (s *service) journalEnded(task protocol.TaskID, j *journal) {
	if err := s.state.closeJournal(task, func(rec *taskRecord) {
		rec.Seq = j.lastSeq()
		rec.Ended = true
		rec.Harness = nil
	}); err != nil {
		s.log.Error("record the end of a task", "task", task, "error", err)
	}
	s.discardWorkspace(task)
	s.sender.notify(task)
}

// openTaskJournal opens the journal of a task that has no process: the
// one its last process left, or a new one continuing after its last seq
// when the sender has deleted that.
func (s *service) openTaskJournal(task protocol.TaskID) (*journal, error) {
	j, err := s.state.openJournal(task, func(rec taskRecord) (*journal, error) {
		j, _, err := reopenJournal(s.cfg.StateDir, task, s.cfg.Harness.Info(), rec.Seq)
		if errors.Is(err, fs.ErrNotExist) {
			return createJournalAfter(s.cfg.StateDir, task, s.cfg.Harness.Info(), rec.Seq)
		}
		return j, err
	})
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	return j, nil
}

// commandDropMoved is a command the daemon gives itself, never one the
// server sends: drop the task, which the server moved to another daemon
// after declaring this one lost (docs/adr/2026-10-08-daemon-loss.md).
const commandDropMoved protocol.CommandKind = "drop_moved"

// dropMoved has task's worker kill the task's harness, if it runs one,
// and forget the task. A task the daemon has forgotten already has what
// is left of it deleted at once.
func (s *service) dropMoved(task protocol.TaskID) {
	drop := protocol.Command{TaskID: task, Kind: commandDropMoved}
	s.mu.Lock()
	w, running := s.running[task]
	forgotten := false
	switch {
	case running:
		w.queue = append(w.queue, drop)
		select {
		case w.wake <- struct{}{}:
		default:
		}
	case s.state.known(task):
		s.spawnLocked(task, drop)
	default:
		forgotten = true
	}
	s.mu.Unlock()
	if forgotten {
		s.forget(task)
	}
}

// forget deletes everything the daemon keeps of task: its record, its
// journal and its workspace. The record goes first, so that a restart
// part-way through finds no task to recover; a journal it finds without
// a record it sends again, which the server refuses once more.
func (s *service) forget(task protocol.TaskID) {
	if err := s.state.forget(task); err != nil {
		s.log.Error("forget task", "task", task, "error", err)
		return
	}
	if err := os.Remove(JournalPath(s.cfg.StateDir, task)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Error("delete the journal of a forgotten task", "task", task, "error", err)
	}
	if err := deleteWorkspace(s.cfg.StateDir, task); err != nil {
		s.log.Error("delete the workspace of a forgotten task", "task", task, "error", err)
	}
	s.log.Info("task forgotten", "task", task)
}

// discardWorkspace deletes the workspace of task, which has ended for
// good on this daemon: it was stopped, or failed in the daemon's view.
func (s *service) discardWorkspace(task protocol.TaskID) {
	if err := deleteWorkspace(s.cfg.StateDir, task); err != nil {
		s.log.Error("delete the workspace of an ended task", "task", task, "error", err)
	}
}
