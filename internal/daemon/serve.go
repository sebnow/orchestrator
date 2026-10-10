package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/runas"
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
	// StateDir holds the daemon's state and the task journals, and the
	// task workspaces unless WorkspaceDir is set.
	StateDir string
	// WorkspaceDir holds the task workspaces; empty means "workspaces"
	// under StateDir. With a HarnessUser it must exist, and that user
	// must own it.
	WorkspaceDir string
	// HarnessUser runs the harness and every command that touches a
	// workspace (docs/adr/2026-10-08-harness-user.md); the zero User runs
	// them as the daemon's own user.
	HarnessUser runas.User
	Harness     harness.Harness
	Gateway     *Gateway
	Log         *slog.Logger
	// Client makes the requests to the server; nil uses a new client.
	Client *http.Client
	// MinBackoff and MaxBackoff bound the wait between attempts to reach
	// the server; zero means 500 ms and 30 s.
	MinBackoff, MaxBackoff time.Duration
	// ShutdownTimeout bounds stopping the running tasks once ctx ends, and
	// then bounds sending their last events; zero means 30 s.
	ShutdownTimeout time.Duration
	// LockTimeout is how long to wait for another daemon to release the
	// state directory; 0 means the default.
	LockTimeout time.Duration
	// GitName and GitEmail are the identity an agent's own git commits get
	// in a task's clone; either empty uses "orchestrator"
	// <orchestrator@localhost>.
	GitName, GitEmail string

	// processes finds and kills harness processes; nil uses ps and
	// signals. Tests set it so that no real process is touched.
	processes processTable
	// measureSlots computes the slots fact for running harness
	// processes, or reports false when it cannot; nil applies the slots
	// rule to this machine. Tests set it.
	measureSlots func(ctx context.Context, running int64) (int, bool)
	// loginInterval is how often the daemon reads its harness's login
	// between connections; zero means loginRefresh. Tests set it.
	loginInterval time.Duration
	// loginWait is how long a login waits for its code; zero means
	// loginTimeout. Tests set it.
	loginWait time.Duration
}

// slots computes the slots fact with running harness processes.
func (cfg Config) slots(ctx context.Context, running int64) (int, bool) {
	if cfg.measureSlots != nil {
		return cfg.measureSlots(ctx, running)
	}
	return machineSlots(ctx, running)
}

// ParseGitIdentity parses s as "Name <email>", the form -git-identity
// takes, and returns its name and email. It fails when s does not parse
// as a mail address, or parses but has no name, as "jane@example.com" and
// "<jane@example.com>" do.
func ParseGitIdentity(s string) (name, email string, err error) {
	addr, err := mail.ParseAddress(s)
	if err != nil {
		return "", "", err
	}
	if addr.Name == "" {
		return "", "", fmt.Errorf("git identity %q: no name", s)
	}
	return addr.Name, addr.Address, nil
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
	// facts are what the daemon reports about its machine each time it
	// opens its command stream and whenever they change; factsMu guards
	// them.
	factsMu sync.Mutex
	facts   protocol.Facts
	// harnesses counts the harness processes running, and
	// capacityChanged asks watchCapacity to recompute the slots fact.
	harnesses       atomic.Int64
	capacityChanged chan struct{}
	// login is the harness's login the daemon runs, nil for none;
	// loginMu guards it.
	loginMu sync.Mutex
	login   *loginRun
}

// currentFacts returns a copy of the daemon's facts.
func (s *service) currentFacts() protocol.Facts {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	return maps.Clone(s.facts)
}

// updateFacts applies change to the daemon's facts and reports whether
// it changed them.
func (s *service) updateFacts(change func(protocol.Facts)) bool {
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	facts := maps.Clone(s.facts)
	change(facts)
	changed := !maps.Equal(facts, s.facts)
	s.facts = facts
	return changed
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
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("state directory %s: %w", stateDir, err)
	}
	// The harness user clones workspaces from the mirrors in the state
	// directory, which it may pass through by name but not list. Every
	// other file there is the daemon's user's alone, by its own mode or
	// its directory's.
	if cfg.HarnessUser.Other() {
		if err := os.Chmod(stateDir, 0o711); err != nil {
			return fmt.Errorf("state directory %s: %w", stateDir, err)
		}
	}
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
	if cfg.LockTimeout <= 0 {
		cfg.LockTimeout = cfg.ShutdownTimeout + 30*time.Second
	}
	lock, err := lockStateDir(ctx, stateDir, cfg.LockTimeout, cfg.Log)
	if err != nil {
		return err
	}
	defer lock.Close()
	workspaceDir, err := resolveWorkspaceDir(stateDir, cfg.WorkspaceDir, cfg.HarnessUser)
	if err != nil {
		return err
	}
	cfg.WorkspaceDir = workspaceDir
	key, err := loadOrCreateSSHKey(stateDir, cfg.ID)
	if err != nil {
		return err
	}
	run, err := newRunner(cfg.HarnessUser)
	if err != nil {
		return err
	}
	// Without a harness user ssh also offers the keys of the owner's
	// agent, when the daemon has one; with one, the daemon's key alone.
	// ssh checks host keys against the ones the server sends and, without
	// a harness user, the daemon user's own as well.
	home, err := os.UserHomeDir()
	if err != nil && !run.as.Other() {
		cfg.Log.Warn("no home directory; ssh checks host keys against the server's alone", "error", err)
	}
	run.mirrors = newMirrors(stateDir, key.Path, run.as.Other(), knownHostsFiles(stateDir, home, run.as.Other())...)
	if run.as.Other() {
		cfg.Log.Info("running tasks as the harness user", "user", run.as.Name, "git", run.gitCmd, "rm", run.rmCmd, "workspace_dir", workspaceDir, "ssh_agent", run.as.SSHAuthSock != "")
	}

	st, err := loadState(stateDir)
	if err != nil {
		return err
	}
	// No task runs before snd is set: recovery appends to journals
	// without observing, and the sender learns those tasks from the state.
	var snd *sender
	// svc is set before the daemon takes commands, and so before any
	// harness starts.
	var svc *service
	d := New(stateDir, cfg.Harness, cfg.Gateway, func(event protocol.Event) { snd.notify(event.TaskID) })
	d.forward = forwardTo(cfg.Client, cfg.Server, cfg.ID)
	d.workspaces = cfg.WorkspaceDir
	d.runner = run
	harnessFiles, removeHarnessFiles, err := run.harnessFiles(stateDir)
	if err != nil {
		return err
	}
	defer removeHarnessFiles()
	d.harnessFiles = harnessFiles
	d.sessionSeen = func(task protocol.TaskID, session string) {
		if err := st.updateTask(task, func(rec *taskRecord) { rec.Session = session }); err != nil {
			cfg.Log.Error("record harness session", "task", task, "error", err)
		}
	}
	switch {
	case cfg.processes != nil:
		d.processes = cfg.processes
	case run.as.Other():
		d.processes = psTable{terminate: true}
	}
	d.toolsChecked = func(task protocol.TaskID, check harness.ToolCheck) {
		switch {
		case !check.Enforceable():
			cfg.Log.Warn("harness offers tools the task's tool classes do not cover; stopping it before its first turn",
				"task", task, "unclassified", check.Unclassified, "excess", check.Excess)
		case len(check.Unclassified) > 0:
			cfg.Log.Info("harness offers tools no tool class names; the task is not restricted, so it runs", "task", task, "unclassified", check.Unclassified)
		}
	}
	d.harnessStarted = func(task protocol.TaskID, pid int) {
		if svc != nil {
			svc.harnessesChanged(1)
		}
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
		cfg:             cfg,
		daemon:          d,
		state:           st,
		sender:          snd,
		log:             cfg.Log,
		stopping:        stopping,
		running:         map[protocol.TaskID]*worker{},
		applied:         st.lastCommand(),
		facts:           detectFacts(cfg.Harness.Info()),
		capacityChanged: make(chan struct{}, 1),
	}
	s.facts[protocol.FactSSHPublicKey] = key.Blob
	if slots, ok := cfg.slots(ctx, 0); ok {
		s.facts[protocol.FactSlots] = strconv.Itoa(slots)
	}
	svc = s
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
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		s.watchCapacity(ctx)
	}()
	watchedLogin := make(chan struct{})
	go func() {
		defer close(watchedLogin)
		s.watchLogin(ctx)
	}()
	cfg.Log.Info("serving", "server", cfg.Server.String(), "daemon", cfg.ID, "state_dir", stateDir, "facts", s.currentFacts())

	<-ctx.Done()
	<-received
	<-watched
	<-watchedLogin
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
//
// A start for a task the daemon ended for good, because its start failed,
// is the owner's retry: once the server holds every event of the failed
// start, the daemon forgets it and takes the new start. Until then it
// returns an error, so that the stream ends and the command is sent
// again; sending the old events after the new start's would number them
// as the new start's.
func (s *service) accept(command protocol.Command) error {
	task := command.TaskID
	if rec, ok := s.state.record(task); ok && rec.Ended && !rec.Running {
		if rec.Acked < rec.Seq {
			return fmt.Errorf("start_task for task %s, whose failed start the server does not hold yet", task)
		}
		s.log.Info("start_task for a task whose start failed here; starting it afresh", "command", command.ID, "task", task)
		s.forget(task)
	}
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
// and the seq to resume from, and whether it was paused. The journal is
// released to the sender. A task stays resumable however its process
// ended, stopped or failed included, so that the owner can follow it up
// in its session and workspace until dismissing it.
func (s *service) processEnded(task protocol.TaskID, t *Task, stopped bool) {
	st := t.State()
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
	s.harnessesChanged(-1)
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
		return s.failStart(task, j, fmt.Errorf("%s: %w", harnessCannotStart, err))
	}
	limits := PauseLimits(start.PauseLimits)
	if err := limits.validate(); err != nil {
		return s.failStart(task, j, fmt.Errorf("%s: %w", harnessCannotStart, err))
	}
	workdir := s.daemon.workspace(task)
	if err := s.daemon.runner.prepareWorkspace(s.stopping, workdir, task, start.Workspace, s.cfg.GitName, s.cfg.GitEmail); err != nil {
		return s.failStart(task, j, fmt.Errorf("%s: %w", workspaceNotPrepared, err))
	}
	// The settings make the task resumable, so they are recorded once its
	// workspace is ready.
	settings := taskSettings{Prompt: start.Prompt, Model: start.Model, Effort: start.Effort, ToolClasses: start.ToolClasses, SystemPrompt: start.SystemPrompt, Acknowledge: limits.Acknowledge, Cleanup: limits.Cleanup, Tools: start.Tools}
	if err := s.state.updateTask(task, func(rec *taskRecord) { rec.Settings = &settings }); err != nil {
		return s.failStart(task, j, fmt.Errorf("%s: record task settings: %w", harnessCannotStart, err))
	}
	return s.startProcess(task, j, TaskSpec{
		ID:           task,
		Prompt:       start.Prompt,
		Workdir:      workdir,
		Model:        start.Model,
		Effort:       start.Effort,
		ToolClasses:  start.ToolClasses,
		SystemPrompt: start.SystemPrompt,
		Pause:        limits,
		Tools:        start.Tools,
	}, true)
}

// resume starts a new process of task that continues its harness session
// (docs/adr/2026-10-08-task-lifetime.md) with followUp, or, when that is
// empty, with the daemon's words for resuming the task. A task with no
// recorded session starts a new one with its first prompt, followed by
// followUp (docs/adr/2026-10-08-restart-recovery.md). A task that cannot
// be resumed, because its start failed or it ended for good before
// stopped and failed tasks could be followed up, gets a harness_exited
// with exitNoSession, which tells the server to start it afresh next.
// A task the daemon has forgotten cannot be told anything.
func (s *service) resume(task protocol.TaskID, followUp string) (*Task, error) {
	if s.stopping.Err() != nil {
		return nil, errors.New("the daemon is shutting down")
	}
	rec, ok := s.state.record(task)
	if !ok {
		return nil, errors.New("the daemon has forgotten the task")
	}
	if !rec.resumable() {
		j, err := s.openTaskJournal(task)
		if err != nil {
			return nil, err
		}
		s.log.Warn("task cannot be resumed", "task", task, "error", exitNoSession)
		j.appendControl(protocol.KindHarnessExited, protocol.HarnessExited{ExitCode: -1, Error: exitNoSession})
		j.close()
		s.journalEnded(task, j)
		return nil, errors.New(exitNoSession)
	}
	j, err := s.openTaskJournal(task)
	if err != nil {
		return nil, err
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
		Workdir:      s.daemon.workspace(task),
		Model:        rec.Settings.Model,
		Effort:       rec.Settings.Effort,
		ToolClasses:  rec.Settings.ToolClasses,
		SystemPrompt: rec.Settings.SystemPrompt,
		Pause:        rec.Settings.limits(),
		Session:      rec.Session,
		Tools:        rec.Settings.Tools,
	}, false)
	if t == nil {
		return nil, errors.New("the harness did not start")
	}
	return t, nil
}

// startProcess starts a process of task on its open journal j. When the
// harness does not start the journal ends with harness_exited saying why
// and startProcess returns nil. A first start that fails ends the task
// for good, since the server starts such a task afresh; a later one
// leaves the task resumable in its session and workspace.
func (s *service) startProcess(task protocol.TaskID, j *journal, spec TaskSpec, first bool) *Task {
	// The harness outlives the daemon's context; shutting down stops it
	// gracefully.
	t, err := s.daemon.start(context.Background(), j, spec)
	if t == nil {
		s.log.Warn("task did not start", "task", task, "error", err)
		if first {
			s.journalEnded(task, j)
		} else {
			s.journalClosed(task, j)
		}
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

// journalClosed records the end of a process of task that did not start,
// releasing its closed journal j; the task stays resumable.
func (s *service) journalClosed(task protocol.TaskID, j *journal) {
	if err := s.state.closeJournal(task, func(rec *taskRecord) {
		rec.Seq = j.lastSeq()
		rec.Harness = nil
	}); err != nil {
		s.log.Error("record the end of a process", "task", task, "error", err)
	}
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
	if err := s.daemon.deleteWorkspace(task); err != nil {
		s.log.Error("delete the workspace of a forgotten task", "task", task, "error", err)
	}
	s.log.Info("task forgotten", "task", task)
}

// discardWorkspace deletes the workspace of task, which has ended for
// good on this daemon: its first start failed, or it could not be
// resumed.
func (s *service) discardWorkspace(task protocol.TaskID) {
	if err := s.daemon.deleteWorkspace(task); err != nil {
		s.log.Error("delete the workspace of an ended task", "task", task, "error", err)
	}
}

// resolveWorkspaceDir returns the absolute workspace directory: dir, or
// "workspaces" under stateDir when dir is empty. A harness user cannot
// enter the state directory and the daemon cannot create a directory
// that user owns, so with one dir must be given and exist.
func resolveWorkspaceDir(stateDir, dir string, user runas.User) (string, error) {
	if dir == "" {
		if user.Other() {
			return "", errors.New("workspace directory: a harness user needs one outside the state directory")
		}
		return filepath.Join(stateDir, "workspaces"), nil
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("workspace directory: %w", err)
	}
	if !user.Other() {
		return dir, nil
	}
	info, err := os.Stat(dir)
	if err == nil && !info.IsDir() {
		err = errors.New("not a directory")
	}
	if err != nil {
		return "", fmt.Errorf("workspace directory %s: %w; create it, owned by the harness user %s", dir, err, user.Name)
	}
	return dir, nil
}

// exitNoSession is the error of the harness_exited a daemon reports when
// it is asked to continue a task it holds no session or workspace for.
// internal/server words it the same, and starts such a task afresh on its
// next follow-up.
const exitNoSession = harnessCannotStart + ": no harness session to resume"
