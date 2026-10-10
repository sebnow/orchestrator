// Package claude holds every Claude Code-shaped type in the orchestrator:
// the stream-json lines the CLI writes to stdout, the lines the daemon
// writes to its stdin, the process driver that exchanges them, and the
// normaliser the server uses to turn the stdout lines into transcript
// entries.
//
// Formats follow https://code.claude.com/docs/en/headless.md and the Agent
// SDK TypeScript reference, https://code.claude.com/docs/en/agent-sdk/typescript.md,
// as observed with Claude Code 2.1.289 in spikes/mod-vs-stdout/runs.
// The format is partly undocumented and may change, so parsing keeps every
// line verbatim and rejects only what contradicts the fields relied upon.
package claude

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Message types and subtypes the daemon acts on. Other values are valid
// and kept as they are.
const (
	TypeSystem         = "system"
	TypeResult         = "result"
	TypeRateLimitEvent = "rate_limit_event"
	SubtypeInit        = "init"

	SubtypeTaskStarted            = "task_started"
	SubtypeTaskNotification       = "task_notification"
	SubtypeTaskUpdated            = "task_updated"
	SubtypeBackgroundTasksChanged = "background_tasks_changed"
)

// ErrMalformed reports a line that is not a stream-json message, or whose
// known fields do not have the documented shape. The line is still the
// harness's output and should be kept as an opaque record.
var ErrMalformed = errors.New("malformed stream-json line")

// Message is one line of stream-json output.
//
// Raw is the line exactly as read, so fields and message types this
// package does not model survive a round trip: MarshalJSON returns Raw.
type Message struct {
	Type    string
	Subtype string
	// SessionID is empty on messages that carry none, such as
	// control_response.
	SessionID string
	UUID      string
	// UserMessageUUID and UserMessageUUIDs echo the uuid of the stdin user
	// message the turn is answering. Claude Code sets them only for messages
	// sent with a uuid, on the turn's first reply, its thinking_tokens frames
	// and its result.
	UserMessageUUID  string
	UserMessageUUIDs []string
	Raw              json.RawMessage

	body any
}

type envelope struct {
	Type             *string  `json:"type"`
	Subtype          string   `json:"subtype"`
	SessionID        string   `json:"session_id"`
	UUID             string   `json:"uuid"`
	UserMessageUUID  string   `json:"user_message_uuid"`
	UserMessageUUIDs []string `json:"user_message_uuids"`
}

// Parse decodes one stdout line. The line must be a JSON object with a
// string "type". Known message types are decoded further; a known type
// whose relied-upon fields have the wrong JSON type is an error.
func Parse(line []byte) (Message, error) {
	line = bytes.TrimRight(line, "\r\n")
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if env.Type == nil {
		return Message{}, fmt.Errorf("%w: no type", ErrMalformed)
	}
	msg := Message{
		Type:             *env.Type,
		Subtype:          env.Subtype,
		SessionID:        env.SessionID,
		UUID:             env.UUID,
		UserMessageUUID:  env.UserMessageUUID,
		UserMessageUUIDs: env.UserMessageUUIDs,
		Raw:              bytes.Clone(line),
	}

	var err error
	switch {
	case msg.Type == TypeSystem && msg.Subtype == SubtypeInit:
		msg.body, err = decode[Init](line)
	case msg.Type == TypeResult:
		msg.body, err = decode[Result](line)
	case msg.Type == TypeSystem && msg.Subtype == SubtypeTaskStarted:
		msg.body, err = decode[TaskStarted](line)
	case msg.Type == TypeSystem && msg.Subtype == SubtypeTaskNotification:
		msg.body, err = decode[TaskNotification](line)
	case msg.Type == TypeSystem && msg.Subtype == SubtypeTaskUpdated:
		msg.body, err = decode[TaskUpdated](line)
	case msg.Type == TypeSystem && msg.Subtype == SubtypeBackgroundTasksChanged:
		msg.body, err = decode[BackgroundTasksChanged](line)
	case msg.Type == TypeRateLimitEvent:
		var event struct {
			Info *RateLimitInfo `json:"rate_limit_info"`
		}
		if err = json.Unmarshal(line, &event); err == nil && event.Info == nil {
			err = errors.New("no rate_limit_info")
		}
		if err == nil {
			msg.body = *event.Info
		}
	}
	if err != nil {
		return Message{}, fmt.Errorf("%w: %s: %v", ErrMalformed, msg.Type, err)
	}
	return msg, nil
}

func decode[T any](line []byte) (T, error) {
	var v T
	err := json.Unmarshal(line, &v)
	return v, err
}

// MarshalJSON returns the line as it was read.
func (m Message) MarshalJSON() ([]byte, error) {
	return m.Raw, nil
}

// Answering returns the uuids of the stdin user messages this message
// shows the turn answering, or nil when it carries no echo.
func (m Message) Answering() []string {
	if len(m.UserMessageUUIDs) > 0 {
		return m.UserMessageUUIDs
	}
	if m.UserMessageUUID != "" {
		return []string{m.UserMessageUUID}
	}
	return nil
}

// Init returns the system/init message. In the spike runs Claude Code
// emitted one at the start of every turn, not only the first.
func (m Message) Init() (Init, bool) {
	v, ok := m.body.(Init)
	return v, ok
}

// Result returns the result message, which Claude Code writes when the
// model stops. It ends the turn unless a subagent runs on in the
// background (see turn in process.go).
func (m Message) Result() (Result, bool) {
	v, ok := m.body.(Result)
	return v, ok
}

// RateLimit returns the rate_limit_info of a rate_limit_event.
func (m Message) RateLimit() (RateLimitInfo, bool) {
	v, ok := m.body.(RateLimitInfo)
	return v, ok
}

// TaskStarted returns the system/task_started message.
func (m Message) TaskStarted() (TaskStarted, bool) {
	v, ok := m.body.(TaskStarted)
	return v, ok
}

// TaskNotification returns the system/task_notification message.
func (m Message) TaskNotification() (TaskNotification, bool) {
	v, ok := m.body.(TaskNotification)
	return v, ok
}

// TaskUpdated returns the system/task_updated message.
func (m Message) TaskUpdated() (TaskUpdated, bool) {
	v, ok := m.body.(TaskUpdated)
	return v, ok
}

// BackgroundTasksChanged returns the system/background_tasks_changed
// message.
func (m Message) BackgroundTasksChanged() (BackgroundTasksChanged, bool) {
	v, ok := m.body.(BackgroundTasksChanged)
	return v, ok
}

// Init is the session description at the start of a turn.
type Init struct {
	Model             string      `json:"model"`
	ClaudeCodeVersion string      `json:"claude_code_version"`
	Cwd               string      `json:"cwd"`
	PermissionMode    string      `json:"permissionMode"`
	Tools             []string    `json:"tools"`
	MCPServers        []MCPServer `json:"mcp_servers"`
	Capabilities      []string    `json:"capabilities"`
}

type MCPServer struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// Result ends a turn. Subtype is "success" or one of the "error_*"
// subtypes; Message.SessionID names the session.
type Result struct {
	IsError bool `json:"is_error"`
	// Result is the final reply text; empty on error subtypes, where
	// Claude Code writes null.
	Result         string   `json:"result"`
	Errors         []string `json:"errors"`
	StopReason     string   `json:"stop_reason"`
	TerminalReason string   `json:"terminal_reason"`
	// NumTurns does not count user turns or model requests in the spike
	// runs; its meaning is undocumented.
	NumTurns      int   `json:"num_turns"`
	DurationMS    int64 `json:"duration_ms"`
	DurationAPIMS int64 `json:"duration_api_ms"`
	// TotalCostUSD is cumulative for the process, at list price, so a
	// turn's cost is the difference from the previous result.
	TotalCostUSD      float64            `json:"total_cost_usd"`
	Usage             Usage              `json:"usage"`
	PermissionDenials []PermissionDenial `json:"permission_denials"`
	// ModelUsage is keyed by the models the process used, its subagents'
	// included.
	ModelUsage map[string]ModelUsage `json:"modelUsage"`
}

// ModelUsage is what a result says of one model the process used.
// ContextWindow is the model's context window in tokens; 0 when absent.
type ModelUsage struct {
	ContextWindow int64 `json:"contextWindow"`
}

// Usage is the token usage of the turn a result ends.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type PermissionDenial struct {
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// RateLimitInfo is the subscription quota a rate_limit_event reports.
//
// The SDK reference documents Status, ResetsAt and Utilization.
// RateLimitType, the overage fields and UnifiedWindows are undocumented;
// they are what Claude Code 2.1.289 sends under a subscription login, and
// UnifiedWindows is the only source of per-window utilization in it.
// Claude Code sent the event once per process in the spike runs, at the
// first API response.
type RateLimitInfo struct {
	Status                string   `json:"status"`
	ResetsAt              int64    `json:"resetsAt"`
	Utilization           *float64 `json:"utilization"`
	RateLimitType         string   `json:"rateLimitType"`
	OverageStatus         string   `json:"overageStatus"`
	OverageDisabledReason string   `json:"overageDisabledReason"`
	IsUsingOverage        bool     `json:"isUsingOverage"`
	// UnifiedWindows maps a window name, such as "five_hour" or
	// "seven_day", to its state.
	UnifiedWindows map[string]RateLimitWindow `json:"unifiedWindows"`
}

type RateLimitWindow struct {
	// Utilization is the fraction of the window used, from 0 to 1.
	Utilization float64 `json:"utilization"`
	// ResetsAt is in Unix seconds.
	ResetsAt int64 `json:"resetsAt"`
}

// Task types of TaskStarted.TaskType.
const TaskTypeLocalAgent = "local_agent"

// The task_* and background_tasks_changed shapes follow the SDK
// TypeScript reference and the runs recorded in
// docs/design/2026-10-09-subagent-stream.md and
// spikes/mod-vs-stdout/runs.

// TaskStarted reports a task Claude Code started: a Bash command or
// Monitor watch ("local_bash"), a subagent ("local_agent") or a
// "remote_agent". Every task_* message names the task by TaskID;
// ToolUseID, when set, is the tool call that started it.
type TaskStarted struct {
	TaskID         string `json:"task_id"`
	ToolUseID      string `json:"tool_use_id"`
	TaskType       string `json:"task_type"`
	IsBackgrounded bool   `json:"is_backgrounded"`
	// Ambient marks a task that is not part of the session's work.
	Ambient bool `json:"ambient"`
}

// TaskNotification reports that a task completed, failed or was stopped.
type TaskNotification struct {
	TaskID    string `json:"task_id"`
	ToolUseID string `json:"tool_use_id"`
	Status    string `json:"status"`
}

// TaskUpdated reports a change to a task; Patch holds only what changed.
type TaskUpdated struct {
	TaskID string `json:"task_id"`
	Patch  struct {
		// Status is one of "pending", "running", "completed", "failed"
		// and "killed".
		Status string `json:"status"`
	} `json:"patch"`
}

// Ended reports whether the update gives the task a terminal status.
func (u TaskUpdated) Ended() bool {
	switch u.Patch.Status {
	case "completed", "failed", "killed":
		return true
	}
	return false
}

// BackgroundTasksChanged lists every live background task.
type BackgroundTasksChanged struct {
	Tasks []BackgroundTask `json:"tasks"`
}

type BackgroundTask struct {
	TaskID   string `json:"task_id"`
	TaskType string `json:"task_type"`
}
