// Package daemon runs tasks on one machine: it drives a harness through
// the harness-neutral contract, hosts the MCP gateway its agents call, and
// journals every event of a task before acting on it.
package daemon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// ErrTaskExists reports a task id that already has a journal. Ids name
// tasks for good, so the start must be refused.
var ErrTaskExists = errors.New("task already has a journal")

// JournalPath returns where the journal of task lives under stateDir.
func JournalPath(stateDir string, task protocol.TaskID) string {
	return filepath.Join(stateDir, "journal", string(task)+".jsonl")
}

// journal appends one task's events to its JSONL file and assigns their
// sequence numbers. The file's line order is the seq order. After a failed
// write every append fails: a gap or a torn line would break replay.
type journal struct {
	task    protocol.TaskID
	harness protocol.Harness

	mu   sync.Mutex
	file *os.File
	seq  uint64
	err  error
}

// createJournal creates the journal of a new task. Writes are not synced
// to disk: the journal protects against a dropped connection or a daemon
// restart, which the page cache survives.
func createJournal(stateDir string, task protocol.TaskID, harness protocol.Harness) (*journal, error) {
	path := JournalPath(stateDir, task)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("%w: %s", ErrTaskExists, task)
	}
	if err != nil {
		return nil, fmt.Errorf("create journal: %w", err)
	}
	return &journal{task: task, harness: harness, file: file}, nil
}

// appendOutput journals one line of harness output. A line that is not
// JSON is journaled as a JSON string so the envelope stays valid.
func (j *journal) appendOutput(line []byte) (protocol.Event, error) {
	if json.Valid(line) {
		return j.append(protocol.KindHarnessOutput, line)
	}
	quoted, err := json.Marshal(string(line))
	if err != nil {
		return protocol.Event{}, err
	}
	return j.append(protocol.KindHarnessOutput, quoted)
}

// appendControl journals a control event the daemon originates.
func (j *journal) appendControl(kind protocol.Kind, payload any) (protocol.Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return protocol.Event{}, fmt.Errorf("encode %s: %w", kind, err)
	}
	return j.append(kind, raw)
}

func (j *journal) append(kind protocol.Kind, payload json.RawMessage) (protocol.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err != nil {
		return protocol.Event{}, j.err
	}
	event := protocol.Event{
		TaskID:  j.task,
		Seq:     j.seq + 1,
		Kind:    kind,
		Harness: j.harness,
		Time:    time.Now().UTC(),
		Payload: payload,
	}
	var line bytes.Buffer
	enc := json.NewEncoder(&line)
	// Harness output stays byte for byte as the harness wrote it.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(event); err != nil {
		return protocol.Event{}, fmt.Errorf("encode event: %w", err)
	}
	if _, err := j.file.Write(line.Bytes()); err != nil {
		j.err = fmt.Errorf("journal %s broken: %w", j.task, err)
		return protocol.Event{}, j.err
	}
	j.seq = event.Seq
	return event, nil
}

func (j *journal) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.err == nil {
		j.err = fmt.Errorf("journal %s closed", j.task)
	}
	return j.file.Close()
}
