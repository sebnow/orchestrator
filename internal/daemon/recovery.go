package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// The errors of the harness_exited event a restarted daemon writes for
// each task whose turn its previous process left unfinished
// (docs/adr/2026-10-08-restart-recovery.md). The server reads the first
// two as a task paused for the owner to resume, and words them the same
// in internal/server; any other error fails the task.
const (
	// restartError: Resume continues the task's recorded session.
	restartError = "daemon restarted during the turn"
	// restartNoSessionError: no session was recorded, so Resume starts a
	// new one with the task's first prompt.
	restartNoSessionError = "daemon restarted during the turn, before the harness reported a session"
	// restartLostError: the task cannot be resumed, because its start
	// had not finished or it had already ended.
	restartLostError = "daemon restarted and the task cannot be resumed"
)

// The errors of the harness_exited event a stopping daemon writes for each
// task whose turn its shutdown cut short, when the task can be resumed
// (docs/adr/2026-10-08-shutdown-recovery.md). The server reads them as it
// reads restartError and restartNoSessionError, and words them the same in
// internal/server.
const (
	stopError          = "daemon stopped during the turn"
	stopNoSessionError = "daemon stopped during the turn, before the harness reported a session"
)

// The errors of the harness_exited event a daemon writes for a turn the
// owner's interrupt cut short, when the task can be resumed. Claude Code
// exits 1 after an interrupted turn
// (docs/design/2026-10-08-interrupt-findings.md), which would fail the
// task, whereas an interrupt is to leave the task's session alive
// (docs/adr/2026-10-07-client-protocol.md). The server reads them as it
// reads stopError and stopNoSessionError, and words them the same in
// internal/server.
const (
	interruptError          = "interrupted by the owner"
	interruptNoSessionError = "interrupted by the owner, before the harness reported a session"
)

// stopText is the harness_exited error a stopping daemon gives for a turn
// it cut short, given the task's record and the session its process last
// reported. It is empty for a task that cannot be resumed, which the
// harness's own exit then ends.
func stopText(rec taskRecord, session string) string {
	return cutShortText(rec, session, stopError, stopNoSessionError)
}

// interruptText is stopText for a turn the owner's interrupt cut short.
func interruptText(rec taskRecord, session string) string {
	return cutShortText(rec, session, interruptError, interruptNoSessionError)
}

// cutShortText is withSession, or withoutSession when no session was
// reported or recorded, for a task that can be resumed, and empty for any
// other.
func cutShortText(rec taskRecord, session, withSession, withoutSession string) string {
	if session != "" {
		rec.Session = session
	}
	switch {
	case !rec.resumable():
		return ""
	case rec.Session == "":
		return withoutSession
	default:
		return withSession
	}
}

// restartExit is the harness_exited a restarted daemon writes for a task
// whose turn its previous process cut short, given the task's record.
func restartExit(rec taskRecord) protocol.HarnessExited {
	exit := protocol.HarnessExited{ExitCode: -1, Error: restartLostError}
	switch {
	case !rec.resumable():
	case rec.Session == "":
		exit.Error = restartNoSessionError
	default:
		exit.Error = restartError
	}
	return exit
}

// recoverTasks closes the record of every task a previous daemon process
// left in the middle of a turn. It first waits up to wait for the harness
// processes that previous process left running to exit, and kills those
// that do not (reapOrphans), so that the harness has exited before its
// turn is reported cut short (docs/adr/2026-10-08-shutdown-recovery.md).
// Each journal
// that does not end in harness_exited gets one, with ExitCode -1 and an
// Error from restartExit. So does a task whose record says a process was
// running or starting, unless its journal ends in that process's own
// exit, which ends the task for good unless it was clean. A task that
// can be resumed stays so, marked as cut short by the restart; any other
// ends for good.
// A task the state knows but that has no journal and no event was
// accepted and never started; it gets a journal holding only that event.
// A task with events but no journal and no process is between
// processes, and stays as it is. A journal the state does not know is
// adopted, so that its events are sent.
// Last, the workspace of every task that can never run again is deleted.
//
// A journal that cannot be read is logged and left alone.
func (d *Daemon) recoverTasks(st *state, log *slog.Logger, wait time.Duration) error {
	d.reapOrphans(st, log, wait)
	journals, err := journalTasks(d.stateDir)
	if err != nil {
		return err
	}
	for _, task := range journals {
		if !st.known(task) {
			if err := st.recordAcked(task, 0); err != nil {
				return err
			}
		}
	}
	for _, task := range st.tasks() {
		rec, _ := st.record(task)
		j, end, err := reopenJournal(d.stateDir, task, d.harness.Info(), rec.Seq)
		if errors.Is(err, fs.ErrNotExist) {
			if !rec.Running && (rec.Seq > 0 || rec.Acked > 0) {
				continue
			}
			j, err = createJournalAfter(d.stateDir, task, d.harness.Info(), rec.Seq)
		}
		if err != nil {
			log.Error("recover task", "task", task, "error", err)
			continue
		}
		cutShort, exitedCleanly := false, false
		resumable := rec.resumable()
		// A journal that ends in harness_exited after the seq recorded when
		// the running process started ends with that process's own exit,
		// which the previous daemon journaled but did not record.
		// Otherwise it may end with the process before the one that was
		// starting.
		ownExit := rec.Running && end.exited && end.seq > rec.Seq
		switch {
		case ownExit:
			exitedCleanly = end.exit != nil && end.exit.ExitCode == 0 && end.exit.Error == ""
		case !end.exited || rec.Running:
			exit := restartExit(rec)
			if _, err := j.appendControl(protocol.KindHarnessExited, exit); err != nil {
				log.Error("recover task", "task", task, "error", err)
			} else {
				cutShort = true
				log.Info("closed the turn the previous daemon left", "task", task, "resumable", resumable, "error", exit.Error)
			}
		}
		seq := j.lastSeq()
		j.close()
		// The previous daemon may have stopped before recording the end of
		// the task's last process. A stop note stays for the resume, since
		// the prompt that carried it may not have reached the harness.
		if err := st.updateTask(task, func(rec *taskRecord) {
			rec.Seq = max(rec.Seq, seq)
			rec.Running = false
			rec.Harness = nil
			switch {
			case ownExit:
				rec.Ended = rec.Ended || !exitedCleanly
				rec.Paused, rec.StopNote, rec.CutShort, rec.Interrupted = false, "", false, false
			case cutShort:
				rec.Ended = rec.Ended || !resumable
				rec.CutShort, rec.Interrupted = resumable, false
			}
		}); err != nil {
			return err
		}
	}
	d.sweepWorkspaces(st, log)
	return nil
}

// sweepWorkspaces deletes the workspace of every task that can never run
// on this daemon again: one the state does not know, because the daemon
// forgot it or never recorded it, and one whose record cannot be resumed.
// A directory that is not named after a task is left alone.
func (d *Daemon) sweepWorkspaces(st *state, log *slog.Logger) {
	entries, err := os.ReadDir(d.workspaces)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		log.Error("list workspaces", "error", err)
		return
	}
	for _, entry := range entries {
		task, err := protocol.ParseTaskID(entry.Name())
		if err != nil || !entry.IsDir() {
			continue
		}
		if rec, ok := st.record(task); ok && rec.resumable() {
			continue
		}
		if err := d.deleteWorkspace(task); err != nil {
			log.Error("delete the workspace of a task that cannot run again", "task", task, "error", err)
			continue
		}
		log.Info("deleted the workspace of a task that cannot run again", "task", task)
	}
}

// journalTasks lists the tasks with a journal under stateDir.
func journalTasks(stateDir string) ([]protocol.TaskID, error) {
	entries, err := os.ReadDir(journalDir(stateDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list journals: %w", err)
	}
	var tasks []protocol.TaskID
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".jsonl")
		if !ok || !entry.Type().IsRegular() {
			continue
		}
		if task, err := protocol.ParseTaskID(name); err == nil {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}
