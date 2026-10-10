package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// StatePath returns where the daemon keeps its state under stateDir.
func StatePath(stateDir string) string {
	return filepath.Join(stateDir, "state.json")
}

// savedState is the daemon's record on disk, beside the journals.
//
// LastCommand is the id of the last command the daemon applied; it is sent
// as Last-Event-ID so that a restarted daemon is not sent commands it has
// already applied. Tasks holds every task the daemon has accepted and not
// yet forgotten.
type savedState struct {
	LastCommand uint64                         `json:"last_command"`
	Tasks       map[protocol.TaskID]taskRecord `json:"tasks"`
}

// taskRecord is what the daemon keeps about a task across its processes
// (docs/adr/2026-10-08-task-lifetime.md).
type taskRecord struct {
	// Acked is the highest seq up to which the server holds every event of
	// the task.
	Acked uint64 `json:"acked"`
	// Seq is the seq of the task's last event as of the end of its latest
	// process. A journal created after the last one was deleted continues
	// from it.
	Seq uint64 `json:"seq,omitempty"`
	// Session is the harness session a new process resumes.
	Session string `json:"session,omitempty"`
	// Settings start the task's later processes; nil until the task's
	// start has been decoded.
	Settings *taskSettings `json:"settings,omitempty"`
	// Paused says the latest process ended with its pause settled, with
	// StopNote the agent's note.
	Paused   bool   `json:"paused,omitempty"`
	StopNote string `json:"stop_note,omitempty"`
	// Ended says the task's first start failed, or it could not be
	// resumed, so that the server starts it afresh rather than resume it;
	// it is forgotten once the server holds all of it.
	Ended bool `json:"ended,omitempty"`
	// CutShort says the daemon cut the latest process's turn short, by
	// dying or by shutting down (docs/adr/2026-10-08-shutdown-recovery.md). Its JSON name
	// predates the Go name, so that state files written before keep their
	// meaning.
	CutShort bool `json:"restarted,omitempty"`
	// Interrupted says the owner's interrupt, rather than the daemon, cut
	// the latest process's turn short; it is set only with CutShort.
	Interrupted bool `json:"interrupted,omitempty"`
	// Harness is the harness process the task's latest process started,
	// until the end of that process is recorded. Found set on start, it is
	// a harness the previous daemon may have left running.
	Harness *harnessProcess `json:"harness,omitempty"`
	// Running says a process of the task holds its journal. Found set on
	// start, it is a process the previous daemon left behind, even one
	// that had not yet journaled its start.
	Running bool `json:"running,omitempty"`
}

// taskSettings are the parts of a task's start that every process of the
// task needs. Prompt, the task's first prompt, starts a new session when
// no session was recorded.
type taskSettings struct {
	Prompt       string        `json:"prompt,omitempty"`
	Model        string        `json:"model"`
	Effort       string        `json:"effort,omitempty"`
	ToolClasses  []string      `json:"tool_classes,omitempty"`
	SystemPrompt string        `json:"system_prompt,omitempty"`
	Acknowledge  time.Duration `json:"pause_acknowledge"`
	Cleanup      time.Duration `json:"pause_cleanup"`
	// Tools are the start's gateway tools; nil, as in the record of a task
	// started before starts carried them, allows every one.
	Tools []string `json:"tools,omitzero"`
}

func (s taskSettings) limits() PauseLimits {
	return PauseLimits{Acknowledge: s.Acknowledge, Cleanup: s.Cleanup}
}

// resumable reports whether a new process may continue the task: in its
// recorded session, or in a new session started with its first prompt
// when none was recorded.
func (r taskRecord) resumable() bool {
	return !r.Ended && r.Settings != nil && (r.Session != "" || r.Settings.Prompt != "")
}

// UnmarshalJSON also reads the record of a daemon before task records
// existed, which was the acknowledged seq alone.
func (r *taskRecord) UnmarshalJSON(data []byte) error {
	var acked uint64
	if json.Unmarshal(data, &acked) == nil {
		*r = taskRecord{Acked: acked}
		return nil
	}
	type plain taskRecord
	return json.Unmarshal(data, (*plain)(r))
}

// state guards savedState and writes every change through to disk. The
// file is replaced by rename, so a reader sees the old or the new state,
// never a mix. Like the journals it is not synced: it protects against a
// daemon restart, which the page cache survives.
//
// It also knows which tasks have a journal open for a running process,
// so that the sender never deletes a journal a process is appending to.
type state struct {
	path string

	mu    sync.Mutex
	saved savedState
	open  map[protocol.TaskID]bool
}

// loadState reads the state under stateDir. A missing file is an empty
// state.
func loadState(stateDir string) (*state, error) {
	s := &state{
		path:  StatePath(stateDir),
		saved: savedState{Tasks: map[protocol.TaskID]taskRecord{}},
		open:  map[protocol.TaskID]bool{},
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read daemon state: %w", err)
	}
	if err := json.Unmarshal(data, &s.saved); err != nil {
		return nil, fmt.Errorf("read daemon state %s: %w", s.path, err)
	}
	if s.saved.Tasks == nil {
		s.saved.Tasks = map[protocol.TaskID]taskRecord{}
	}
	return s, nil
}

func (s *state) lastCommand() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved.LastCommand
}

// known reports whether the daemon has accepted task and not forgotten it.
func (s *state) known(task protocol.TaskID) bool {
	_, ok := s.record(task)
	return ok
}

func (s *state) record(task protocol.TaskID) (taskRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.saved.Tasks[task]
	return rec, ok
}

func (s *state) acked(task protocol.TaskID) uint64 {
	rec, _ := s.record(task)
	return rec.Acked
}

// tasks returns the known tasks in id order.
func (s *state) tasks() []protocol.TaskID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.saved.Tasks))
}

// recordCommand records that command id has been applied.
func (s *state) recordCommand(id uint64) error {
	return s.update(func(saved *savedState) { saved.LastCommand = id })
}

// recordStart records, in one write, that the start_task command id has
// been applied and that task is now known.
func (s *state) recordStart(id uint64, task protocol.TaskID) error {
	return s.update(func(saved *savedState) {
		saved.LastCommand = id
		saved.Tasks[task] = taskRecord{}
	})
}

// recordAcked sets the acknowledged seq of task, making it known.
func (s *state) recordAcked(task protocol.TaskID, seq uint64) error {
	return s.updateTask(task, func(rec *taskRecord) { rec.Acked = seq })
}

// recordAckedIfKnown sets the acknowledged seq of task unless the daemon
// has forgotten it, which an acknowledgement must not undo.
func (s *state) recordAckedIfKnown(task protocol.TaskID, seq uint64) error {
	return s.update(func(saved *savedState) {
		if rec, ok := saved.Tasks[task]; ok {
			rec.Acked = seq
			saved.Tasks[task] = rec
		}
	})
}

// updateTask changes the record of task, creating it when missing.
func (s *state) updateTask(task protocol.TaskID, change func(*taskRecord)) error {
	return s.update(func(saved *savedState) {
		rec := saved.Tasks[task]
		change(&rec)
		saved.Tasks[task] = rec
	})
}

// openJournal runs open, which opens or creates the journal of task, and
// records that a process holds the journal until closeJournal. The record
// is saved first, so that a daemon that stops while the process starts
// leaves a mark for recovery.
func (s *state) openJournal(task protocol.TaskID, open func(taskRecord) (*journal, error)) (*journal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.open[task] {
		return nil, fmt.Errorf("task %s already has a process", task)
	}
	err := s.updateLocked(func(saved *savedState) {
		rec := saved.Tasks[task]
		rec.Running = true
		saved.Tasks[task] = rec
	})
	if err != nil {
		return nil, err
	}
	j, err := open(s.saved.Tasks[task])
	if err != nil {
		s.updateLocked(func(saved *savedState) {
			rec := saved.Tasks[task]
			rec.Running = false
			saved.Tasks[task] = rec
		})
		return nil, err
	}
	s.open[task] = true
	return j, nil
}

// closeJournal records the end of task's process with change and releases
// its journal. The journal is released even when the change cannot be
// saved; the record then keeps its last saved seq, which stops the sender
// deleting the journal.
func (s *state) closeJournal(task protocol.TaskID, change func(*taskRecord)) error {
	err := s.updateTask(task, func(rec *taskRecord) {
		change(rec)
		rec.Running = false
	})
	s.mu.Lock()
	delete(s.open, task)
	s.mu.Unlock()
	return err
}

// dropJournal deletes the journal at path of a task the server holds up to
// acked, and reports whether the sender is done with the task. A journal
// held by a process, or with events after acked, stays. A task that can
// never be resumed is forgotten; it leaves the state before its journal
// is deleted, because a journal the state does not know is adopted on
// restart and only sent again, which the server ignores.
func (s *state) dropJournal(task protocol.TaskID, acked uint64, path string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.saved.Tasks[task]
	if !ok {
		return true, nil
	}
	if s.open[task] || rec.Seq != acked {
		return false, nil
	}
	if !rec.resumable() {
		if err := s.updateLocked(func(saved *savedState) { delete(saved.Tasks, task) }); err != nil {
			return false, err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	return true, nil
}

// forget removes task from the state, whatever its record says.
func (s *state) forget(task protocol.TaskID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.open, task)
	if _, ok := s.saved.Tasks[task]; !ok {
		return nil
	}
	return s.updateLocked(func(saved *savedState) { delete(saved.Tasks, task) })
}

// update applies change and writes the result. If the write fails the
// state is left as it was.
func (s *state) update(change func(*savedState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateLocked(change)
}

func (s *state) updateLocked(change func(*savedState)) error {
	next := savedState{LastCommand: s.saved.LastCommand, Tasks: maps.Clone(s.saved.Tasks)}
	change(&next)
	data, err := json.Marshal(next)
	if err != nil {
		return fmt.Errorf("encode daemon state: %w", err)
	}
	if err := writeFileAtomic(s.path, data); err != nil {
		return fmt.Errorf("write daemon state: %w", err)
	}
	s.saved = next
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}
