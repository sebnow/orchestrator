package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// CommandKind says what a command asks the daemon to do and what its
// payload holds.
type CommandKind string

const (
	// CommandStartTask: StartTask.
	CommandStartTask CommandKind = "start_task"
	// CommandPrompt: Prompt, a follow-up prompt to a running task.
	CommandPrompt CommandKind = "prompt"
	// CommandPause asks the agent to pause cooperatively. No payload.
	CommandPause CommandKind = "pause"
	// CommandResume resumes a paused task in place. No payload.
	CommandResume CommandKind = "resume"
	// CommandInterrupt is the emergency stop of the current turn; the
	// task's session stays alive. No payload.
	CommandInterrupt CommandKind = "interrupt"
	// CommandStop ends the task and its harness process. No payload.
	CommandStop CommandKind = "stop"
	// CommandAnswerPermission: AnswerPermission.
	CommandAnswerPermission CommandKind = "answer_permission"
)

// Command is one entry in a daemon's command log. ID increases with every
// command the server issues and stays the same when the command is sent
// again, so the daemon can ignore an ID it has already applied.
type Command struct {
	ID       uint64          `json:"id"`
	DaemonID DaemonID        `json:"daemon_id"`
	TaskID   TaskID          `json:"task_id"`
	Kind     CommandKind     `json:"kind"`
	Time     time.Time       `json:"time"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// StartTask starts a task on the daemon
// (docs/adr/2026-10-07-task-interface.md). Without a Workspace the daemon
// prepares an empty directory. An empty Model names no model.
type StartTask struct {
	Prompt       string      `json:"prompt"`
	SystemPrompt string      `json:"system_prompt,omitempty"`
	Workspace    *Workspace  `json:"workspace,omitempty"`
	Model        string      `json:"model,omitempty"`
	PauseLimits  PauseLimits `json:"pause_limits"`
}

// Workspace names the repository and ref a task works in.
type Workspace struct {
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
}

// PauseLimits bound a cooperative pause
// (docs/adr/2026-10-07-graceful-pause.md): the daemon interrupts the
// harness if the agent has not acknowledged a pause within Acknowledge, or
// has not finished within Cleanup of acknowledging it.
//
// On the wire both are Go duration strings, such as "2m0s".
type PauseLimits struct {
	Acknowledge time.Duration
	Cleanup     time.Duration
}

type pauseLimitsJSON struct {
	Acknowledge string `json:"acknowledge"`
	Cleanup     string `json:"cleanup"`
}

func (l PauseLimits) MarshalJSON() ([]byte, error) {
	return json.Marshal(pauseLimitsJSON{Acknowledge: l.Acknowledge.String(), Cleanup: l.Cleanup.String()})
}

func (l *PauseLimits) UnmarshalJSON(data []byte) error {
	var raw pauseLimitsJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	acknowledge, err := time.ParseDuration(raw.Acknowledge)
	if err != nil {
		return fmt.Errorf("pause_limits.acknowledge: %w", err)
	}
	cleanup, err := time.ParseDuration(raw.Cleanup)
	if err != nil {
		return fmt.Errorf("pause_limits.cleanup: %w", err)
	}
	*l = PauseLimits{Acknowledge: acknowledge, Cleanup: cleanup}
	return nil
}

// Prompt is a follow-up prompt to a running task.
type Prompt struct {
	Text string `json:"text"`
}

// AnswerPermission answers the PermissionRequested event that carried
// RequestID. Message tells the agent why when the request is denied.
type AnswerPermission struct {
	RequestID string `json:"request_id"`
	Allow     bool   `json:"allow"`
	Message   string `json:"message,omitempty"`
}
