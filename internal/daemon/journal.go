// Package daemon runs tasks on one machine: it drives a harness through
// the harness-neutral contract, hosts the MCP gateway its agents call, and
// journals every event of a task before acting on it.
package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	return filepath.Join(journalDir(stateDir), string(task)+".jsonl")
}

func journalDir(stateDir string) string {
	return filepath.Join(stateDir, "journal")
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

// journalEnd describes the last complete event of a journal.
type journalEnd struct {
	seq     uint64
	harness protocol.Harness
	// exited is true when the last event is harness_exited, which ends the
	// task's stream.
	exited bool
	// size is the length of the complete lines, in bytes.
	size int64
}

// scanJournal reads the complete lines of the journal at path. A last line
// without its newline is a write the daemon did not finish; it is not
// counted. A complete line that is not an event, or a seq out of order,
// is an error.
func scanJournal(path string) (journalEnd, error) {
	file, err := os.Open(path)
	if err != nil {
		return journalEnd{}, err
	}
	defer file.Close()
	var end journalEnd
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return end, nil
		}
		if err != nil {
			return journalEnd{}, fmt.Errorf("read journal %s: %w", path, err)
		}
		var event protocol.Event
		if err := json.Unmarshal(line, &event); err != nil {
			return journalEnd{}, fmt.Errorf("journal %s at byte %d: %w", path, end.size, err)
		}
		if event.Seq != end.seq+1 {
			return journalEnd{}, fmt.Errorf("journal %s at byte %d: seq %d follows %d", path, end.size, event.Seq, end.seq)
		}
		end = journalEnd{seq: event.Seq, harness: event.Harness, exited: event.Kind == protocol.KindHarnessExited, size: end.size + int64(len(line))}
	}
}

// reopenJournal opens task's existing journal to append after its last
// complete event, cutting off a torn last line. Appended events carry the
// harness of the last event, or fallback when the journal is empty.
func reopenJournal(stateDir string, task protocol.TaskID, fallback protocol.Harness) (*journal, journalEnd, error) {
	path := JournalPath(stateDir, task)
	end, err := scanJournal(path)
	if err != nil {
		return nil, journalEnd{}, err
	}
	if err := os.Truncate(path, end.size); err != nil {
		return nil, journalEnd{}, fmt.Errorf("cut torn line from journal %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, journalEnd{}, fmt.Errorf("open journal: %w", err)
	}
	harness := end.harness
	if end.seq == 0 {
		harness = fallback
	}
	return &journal{task: task, harness: harness, file: file, seq: end.seq}, end, nil
}
