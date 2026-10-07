package daemon

import (
	"context"
	"errors"
	"fmt"
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
	if err := d.recoverTasks(st, cfg.Log); err != nil {
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
	w := &worker{wake: make(chan struct{}, 1)}
	s.mu.Lock()
	s.running[task] = w
	s.applied = command.ID
	s.mu.Unlock()
	s.workers.Go(func() { s.work(task, w, command) })
	return nil
}

// route queues a command for its task's worker, or logs and skips it when
// the daemon is not running the task.
func (s *service) route(command protocol.Command) {
	s.mu.Lock()
	w, ok := s.running[command.TaskID]
	if ok {
		w.queue = append(w.queue, command)
		select {
		case w.wake <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
	if !ok {
		s.log.Warn("command for a task this daemon is not running; skipped",
			"command", command.ID, "kind", command.Kind, "task", command.TaskID)
	}
	s.recordApplied(command.ID)
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

// work starts the task, then applies its commands until it ends or the
// daemon shuts down, when it stops the task.
func (s *service) work(task protocol.TaskID, w *worker, start protocol.Command) {
	t := s.startTask(start)
	var done <-chan struct{}
	if t != nil {
		done = t.Done()
	}
	for t != nil {
		select {
		case <-w.wake:
		case <-done:
			t = nil
			continue
		case <-s.stopping.Done():
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
			t.Stop(ctx)
			cancel()
			t = nil
			continue
		}
		s.mu.Lock()
		commands := w.queue
		w.queue = nil
		s.mu.Unlock()
		for _, command := range commands {
			s.apply(t, command)
		}
	}

	s.mu.Lock()
	delete(s.running, task)
	skipped := w.queue
	s.mu.Unlock()
	for _, command := range skipped {
		s.log.Warn("command for a task that has ended; skipped", "command", command.ID, "kind", command.Kind, "task", task)
	}
}

// startTask prepares the task's workspace and starts its harness. When
// either fails the task's journal ends with harness_exited saying why, and
// startTask returns nil.
func (s *service) startTask(command protocol.Command) *Task {
	task := command.TaskID
	j, err := createJournal(s.cfg.StateDir, task, s.cfg.Harness.Info())
	if err != nil {
		// Without a journal the server cannot be told; a restart ends the
		// task, which the state knows.
		s.log.Error("start task", "task", task, "error", err)
		return nil
	}
	fail := func(err error) *Task {
		s.log.Warn("task did not start", "task", task, "error", err)
		event, err := j.appendControl(protocol.KindHarnessExited, protocol.HarnessExited{ExitCode: -1, Error: err.Error()})
		if err == nil {
			s.sender.notify(event.TaskID)
		}
		j.close()
		return nil
	}
	start, err := decodePayload[protocol.StartTask](command)
	if err != nil {
		return fail(err)
	}
	limits := PauseLimits(start.PauseLimits)
	if err := limits.validate(); err != nil {
		return fail(err)
	}
	workdir := workspacePath(s.cfg.StateDir, task)
	if err := prepareWorkspace(s.stopping, workdir, start.Workspace); err != nil {
		return fail(fmt.Errorf("prepare workspace: %w", err))
	}
	// The harness outlives the daemon's context; shutting down stops it
	// gracefully.
	t, err := s.daemon.start(context.Background(), j, TaskSpec{
		ID:           task,
		Prompt:       start.Prompt,
		Workdir:      workdir,
		Model:        start.Model,
		SystemPrompt: start.SystemPrompt,
		Pause:        limits,
	})
	if err != nil {
		s.log.Warn("task did not start", "task", task, "error", err)
		return t
	}
	s.log.Info("task started", "task", task, "workdir", workdir)
	return t
}
