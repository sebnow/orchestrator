package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// restartError is the error of the harness_exited event a restarted
// daemon writes for each task the previous process left running.
const restartError = "daemon restarted"

// recoverTasks ends every task a previous daemon process left unfinished.
// The harness exits when the daemon does, so none of those tasks can be
// running; each journal that does not end in harness_exited gets one,
// with ExitCode -1 and Error "daemon restarted". A task the state knows
// but that has no journal was accepted and never started; it gets a
// journal holding only that event. A journal the state does not know is
// adopted, so that its events are sent.
//
// A journal that cannot be read is logged and left alone.
func (d *Daemon) recoverTasks(st *state, log *slog.Logger) error {
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
	exit := protocol.HarnessExited{ExitCode: -1, Error: restartError}
	for _, task := range st.tasks() {
		j, end, err := reopenJournal(d.stateDir, task, d.harness.Info())
		if errors.Is(err, fs.ErrNotExist) {
			j, err = createJournal(d.stateDir, task, d.harness.Info())
		}
		if err != nil {
			log.Error("recover task", "task", task, "error", err)
			continue
		}
		if !end.exited {
			if _, err := j.appendControl(protocol.KindHarnessExited, exit); err != nil {
				log.Error("recover task", "task", task, "error", err)
			} else {
				log.Info("ended task left by the previous daemon", "task", task)
			}
		}
		j.close()
	}
	return nil
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
