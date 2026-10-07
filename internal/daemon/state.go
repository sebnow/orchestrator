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
// yet forgotten, with the highest seq up to which the server holds every
// event of the task.
type savedState struct {
	LastCommand uint64                     `json:"last_command"`
	Tasks       map[protocol.TaskID]uint64 `json:"tasks"`
}

// state guards savedState and writes every change through to disk. The
// file is replaced by rename, so a reader sees the old or the new state,
// never a mix. Like the journals it is not synced: it protects against a
// daemon restart, which the page cache survives.
type state struct {
	path string

	mu    sync.Mutex
	saved savedState
}

// loadState reads the state under stateDir. A missing file is an empty
// state.
func loadState(stateDir string) (*state, error) {
	s := &state{path: StatePath(stateDir), saved: savedState{Tasks: map[protocol.TaskID]uint64{}}}
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
		s.saved.Tasks = map[protocol.TaskID]uint64{}
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
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.saved.Tasks[task]
	return ok
}

func (s *state) acked(task protocol.TaskID) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved.Tasks[task]
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
		saved.Tasks[task] = 0
	})
}

// recordAcked sets the acknowledged seq of task, making it known.
func (s *state) recordAcked(task protocol.TaskID, seq uint64) error {
	return s.update(func(saved *savedState) { saved.Tasks[task] = seq })
}

// forget drops task from the state.
func (s *state) forget(task protocol.TaskID) error {
	return s.update(func(saved *savedState) { delete(saved.Tasks, task) })
}

// update applies change and writes the result. If the write fails the
// state is left as it was.
func (s *state) update(change func(*savedState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
