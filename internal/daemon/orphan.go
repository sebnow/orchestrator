package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// orphanPoll is how often recovery checks whether a harness the previous
// daemon left behind has exited.
const orphanPoll = 250 * time.Millisecond

// harnessProcess identifies a harness process across daemon restarts.
// Started is the process's start time as the OS reports it; with the pid
// it tells the harness from a later process that reuses its pid.
type harnessProcess struct {
	PID     int    `json:"pid"`
	Started string `json:"started,omitempty"`
}

// processTable looks processes up and kills them.
type processTable interface {
	// started returns the start time of the process pid, or "" when no
	// such process exists.
	started(pid int) (string, error)
	kill(pid int) error
}

// psTable reads start times with ps, which macOS and Linux with procps
// both provide, with second resolution.
type psTable struct{}

func (psTable) started(pid int) (string, error) {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	// The start time is compared across daemon processes, so it must not
	// depend on the daemon's locale.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 && len(strings.TrimSpace(string(out))) == 0 {
		// ps exits 1 when no process has the pid.
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("ps: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (psTable) kill(pid int) error {
	return syscall.Kill(pid, syscall.SIGKILL)
}

// orphan is a harness the previous daemon process started and left
// running.
type orphan struct {
	task    protocol.TaskID
	process harnessProcess
	since   time.Time
}

// reapOrphans deals with the harness processes the previous daemon left
// running, before recovery reports their turns cut short
// (docs/adr/2026-10-08-shutdown-recovery.md): it waits up to wait for
// each to exit and then kills those still running, so that no process the
// daemon resumes shares its session with one. A process whose start time
// was not recorded cannot be told from a later one with its pid, and is
// left alone.
func (d *Daemon) reapOrphans(st *state, log *slog.Logger, wait time.Duration) {
	var running []orphan
	for _, task := range st.tasks() {
		rec, _ := st.record(task)
		if rec.Harness == nil {
			continue
		}
		if rec.Harness.Started == "" {
			log.Warn("harness left by the previous daemon cannot be identified; left alone", "task", task, "pid", rec.Harness.PID)
			continue
		}
		o := orphan{task: task, process: *rec.Harness, since: time.Now()}
		if d.stillRunning(o, log) {
			log.Info("harness left by the previous daemon is still running; waiting for it to exit", "task", task, "pid", o.process.PID, "wait", wait)
			running = append(running, o)
		}
	}
	deadline := time.Now().Add(wait)
	for len(running) > 0 && time.Now().Before(deadline) {
		time.Sleep(min(orphanPoll, time.Until(deadline)))
		running = slices.DeleteFunc(running, func(o orphan) bool {
			if d.stillRunning(o, log) {
				return false
			}
			log.Info("harness left by the previous daemon exited", "task", o.task, "pid", o.process.PID, "after", time.Since(o.since).Round(time.Millisecond))
			return true
		})
	}
	for _, o := range running {
		if err := d.processes.kill(o.process.PID); err != nil {
			log.Error("kill the harness left by the previous daemon", "task", o.task, "pid", o.process.PID, "error", err)
			continue
		}
		log.Warn("harness left by the previous daemon did not exit; killed it", "task", o.task, "pid", o.process.PID, "after", time.Since(o.since).Round(time.Millisecond))
	}
}

// stillRunning reports whether o's process runs: a process has its pid
// and its start time. A lookup that fails counts as not running, so that
// recovery is not held up by it.
func (d *Daemon) stillRunning(o orphan, log *slog.Logger) bool {
	started, err := d.processes.started(o.process.PID)
	if err != nil {
		log.Error("look up the harness left by the previous daemon", "task", o.task, "pid", o.process.PID, "error", err)
		return false
	}
	return started == o.process.Started
}
