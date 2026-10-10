// Package protocol defines the harness-neutral records a daemon and the
// server exchange: the event envelope and the control events the daemon
// originates, and the commands the server issues
// (docs/adr/2026-10-07-client-protocol.md).
//
// Harness output travels as an opaque payload tagged with the harness name
// and version; nothing here depends on a particular harness.
package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TaskID names a task. It appears in file names and URL paths, so it is
// limited to ASCII letters, digits, '.', '_' and '-', and is never "." or
// "..".
type TaskID string

var ErrInvalidTaskID = errors.New("invalid task id")

func ParseTaskID(raw string) (TaskID, error) {
	if !isPathSafeID(raw) {
		return "", fmt.Errorf("%w: %q", ErrInvalidTaskID, raw)
	}
	return TaskID(raw), nil
}

// DaemonID names a daemon. The daemon chooses it, and it appears in URL
// paths, so it follows the same rules as TaskID.
type DaemonID string

var ErrInvalidDaemonID = errors.New("invalid daemon id")

func ParseDaemonID(raw string) (DaemonID, error) {
	if !isPathSafeID(raw) {
		return "", fmt.Errorf("%w: %q", ErrInvalidDaemonID, raw)
	}
	return DaemonID(raw), nil
}

const maxIDLength = 128

func isPathSafeID(raw string) bool {
	if raw == "" || raw == "." || raw == ".." || len(raw) > maxIDLength {
		return false
	}
	for _, r := range raw {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if !valid {
			return false
		}
	}
	return true
}

// Kind says what an event's payload holds.
type Kind string

const (
	// KindHarnessOutput: one line the harness wrote, verbatim. The payload
	// is the line itself when it is JSON, otherwise the line as a JSON
	// string.
	KindHarnessOutput Kind = "harness_output"
	// KindHarnessStarted: HarnessStarted.
	KindHarnessStarted Kind = "harness_started"
	// KindHarnessExited: HarnessExited.
	KindHarnessExited Kind = "harness_exited"
	// KindPermissionRequested: PermissionRequested.
	KindPermissionRequested Kind = "permission_requested"
	// KindPauseAcknowledged: PauseAcknowledged.
	KindPauseAcknowledged Kind = "pause_acknowledged"
	// KindPauseSettled: PauseSettled.
	KindPauseSettled Kind = "pause_settled"
	// KindQuotaObserved: QuotaObserved.
	KindQuotaObserved Kind = "quota_observed"
	// KindBranchPushed: BranchPushed.
	KindBranchPushed Kind = "branch_pushed"
	// KindPromptHeld: PromptHeld.
	KindPromptHeld Kind = "prompt_held"
	// KindPromptReleased: PromptReleased.
	KindPromptReleased Kind = "prompt_released"
)

// PromptHeld reports that the daemon holds the prompt command Prompt
// until the running turn ends, rather than send it to the harness, which
// would queue it where it cannot be withdrawn.
type PromptHeld struct {
	Prompt uint64 `json:"prompt"`
}

// What became of a held prompt.
const (
	// ReleasedSent: the daemon sent it to the harness as the next turn's
	// prompt.
	ReleasedSent = "sent"
	// ReleasedWithdrawn: the owner withdrew it.
	ReleasedWithdrawn = "withdrawn"
	// ReleasedDropped: the turn ended with the task pausing, or its
	// process exited, before it was sent; it never will be.
	ReleasedDropped = "dropped"
)

// PromptReleased reports that the daemon no longer holds the prompt
// command Prompt, and Outcome says why: ReleasedSent, ReleasedWithdrawn
// or ReleasedDropped.
type PromptReleased struct {
	Prompt  uint64 `json:"prompt"`
	Outcome string `json:"outcome"`
}

// Harness identifies the harness that produced a task's events.
type Harness struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Event is one record in a task's stream. Seq starts at 1 and increases by
// one per event of the task, and the daemon journals the event before it
// acts on it.
type Event struct {
	TaskID  TaskID          `json:"task_id"`
	Seq     uint64          `json:"seq"`
	Kind    Kind            `json:"kind"`
	Harness Harness         `json:"harness"`
	Time    time.Time       `json:"time"`
	Payload json.RawMessage `json:"payload"`
}

type HarnessStarted struct {
	PID     int    `json:"pid"`
	Model   string `json:"model"`
	Workdir string `json:"workdir"`
}

// HarnessExited ends a task's stream. ExitCode is -1 when the process
// never started or was killed by a signal; Error then says why. Stderr is
// the end of what the harness wrote to stderr.
type HarnessExited struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
}

// PermissionRequested is the harness asking whether it may run a tool.
// The request waits until AnswerPermission names RequestID.
type PermissionRequested struct {
	RequestID string          `json:"request_id"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
}

// PauseAcknowledged carries the agent's stop note: its own account of
// where it stopped.
type PauseAcknowledged struct {
	Note string `json:"note"`
}

// PauseSettled reports that a pause has taken effect: the turn answering
// the pause request has ended, or no turn was running when the pause
// arrived. Nothing else tells the server this, because the pause request's
// id never leaves the daemon and a pause with no running turn produces no
// harness output at all.
type PauseSettled struct {
	// Interrupted is true when a pause limit passed and the daemon
	// interrupted the harness to settle the pause.
	Interrupted bool `json:"interrupted"`
}

type QuotaStatus string

const (
	QuotaAllowed  QuotaStatus = "allowed"
	QuotaWarning  QuotaStatus = "warning"
	QuotaRejected QuotaStatus = "rejected"
	QuotaUnknown  QuotaStatus = "unknown"
)

// QuotaObserved is the harness reporting the subscription's usage limits.
type QuotaObserved struct {
	Status  QuotaStatus   `json:"status"`
	Windows []QuotaWindow `json:"windows"`
}

// QuotaWindow is one usage-limit window, such as "five_hour".
type QuotaWindow struct {
	Name string `json:"name"`
	// Utilization is the fraction of the window used, from 0 to 1.
	Utilization float64   `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
}

// BranchPushed reports, at the end of a turn, the branch on which a
// task's work leaves the daemon (docs/adr/2026-10-08-work-delivery.md).
// The daemon sends it when the turn leaves the branch holding commits
// the remote's branch lacks, which it pushes, or leaves files
// uncommitted in the workspace, or both; it sends none when neither
// holds. Branch is the branch's name and Commit the commit the branch
// holds in the workspace. Ahead counts the branch's commits beyond the
// ref the task started from, and Uncommitted the files left modified or
// untracked in the workspace. With Error empty, the remote's branch
// holds Commit when Ahead is above zero, pushed by this turn or an
// earlier one; when Ahead is zero the branch holds no work, and the
// daemon pushed nothing. Error says why the branch could not be read or
// pushed.
type BranchPushed struct {
	Branch      string `json:"branch"`
	Commit      string `json:"commit"`
	Ahead       int    `json:"ahead"`
	Uncommitted int    `json:"uncommitted"`
	Error       string `json:"error"`
}
