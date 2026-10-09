// Package transcript holds the harness-neutral, readable form of a task's
// history: what the owner asked, what the agent said and did, and what
// the daemon reported, as one sequence of entries
// (docs/adr/2026-10-07-client-protocol.md, "server-side normalisation").
//
// Entries are derived on read from the stored events and commands, which
// stay the record; nothing here is stored. A harness package turns its
// own output lines into Bodies; nothing here depends on a harness.
package transcript

import (
	"encoding/json"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// Entry is one step in a task's transcript.
type Entry struct {
	Time   time.Time
	Source Source
	Body   Body
}

// Source names the stored record an entry was derived from: an event,
// by task and seq, a command, by id, or a message between tasks, by id.
// Exactly one of Seq, CommandID and MessageID is non-zero. One record may
// yield several entries, which share a Source. TaskID is the task the
// record belongs to, which for a child's start or a message may be
// another task than the one whose transcript holds the entry.
type Source struct {
	TaskID    protocol.TaskID
	Seq       uint64
	CommandID uint64
	MessageID uint64
}

// Kind names the type of an entry's Body.
type Kind string

const (
	KindOwnerPrompt          Kind = "owner_prompt"
	KindPauseRequested       Kind = "pause_requested"
	KindStopRequested        Kind = "stop_requested"
	KindInterrupted          Kind = "interrupted"
	KindPermissionAnswered   Kind = "permission_answered"
	KindAgentText            Kind = "agent_text"
	KindAgentThinking        Kind = "agent_thinking"
	KindToolCall             Kind = "tool_call"
	KindToolResult           Kind = "tool_result"
	KindTurnEnded            Kind = "turn_ended"
	KindPermissionRequested  Kind = "permission_requested"
	KindPauseAcknowledged    Kind = "pause_acknowledged"
	KindPauseSettled         Kind = "pause_settled"
	KindQuotaObserved        Kind = "quota_observed"
	KindHarnessStarted       Kind = "harness_started"
	KindHarnessExited        Kind = "harness_exited"
	KindMessageSent          Kind = "message_sent"
	KindMessageReceived      Kind = "message_received"
	KindChildSpawned         Kind = "child_spawned"
	KindChildEnded           Kind = "child_ended"
	KindMessageUndeliverable Kind = "message_undeliverable"
	KindTaskMoved            Kind = "task_moved"
	KindBranchPushed         Kind = "branch_pushed"
	KindUnknown              Kind = "unknown"
)

// Body is what an entry says. It is one of the types in this package.
type Body interface {
	Kind() Kind
	body()
}

// OwnerPrompt is a prompt the owner sent: the task's first prompt, a
// follow-up, or a resume. A resume carries no text, because the daemon
// chooses its words. SpawnedBy, when set, names the task whose agent
// spawned this one; the first prompt is then that agent's, not the
// owner's.
type OwnerPrompt struct {
	Text      string
	Resume    bool
	SpawnedBy *protocol.TaskID
}

// PauseRequested is the owner asking the agent to pause.
type PauseRequested struct{}

// StopRequested is the owner ending the task.
type StopRequested struct{}

// Interrupted is the owner's emergency stop of the running turn.
type Interrupted struct{}

// PermissionAnswered is the answer to a PermissionRequested with the
// same RequestID, given by whoever By names.
type PermissionAnswered struct {
	RequestID string
	Allow     bool
	Message   string
	By        AnsweredBy
}

// AnsweredBy names who answered a permission request: the owner, or the
// server's permission policy (docs/adr/2026-10-08-permission-policy.md).
type AnsweredBy string

const (
	AnsweredByOwner  AnsweredBy = "owner"
	AnsweredByPolicy AnsweredBy = "policy"
)

// AgentText is text the agent wrote to the owner.
type AgentText struct {
	Text string
	// ParentToolUseID, here and on AgentThinking, ToolCall, ToolResult
	// and Unknown, names the ToolCall that started the harness's own
	// subagent which wrote the entry; it is empty for the task's main
	// conversation. A subagent's entries are nested under that call.
	ParentToolUseID string
}

// AgentThinking is the agent reasoning. Text is empty when the harness
// reports that the agent thought but withholds what.
type AgentThinking struct {
	Text            string
	ParentToolUseID string
}

// ToolCall is the agent calling a tool. Input is the tool's arguments as
// JSON.
type ToolCall struct {
	ID              string
	Name            string
	Input           json.RawMessage
	ParentToolUseID string
}

// ToolResult is what a tool returned to the ToolCall with ID ToolCallID,
// as text. IsError is set when the call failed or was refused.
type ToolResult struct {
	ToolCallID      string
	Content         string
	IsError         bool
	ParentToolUseID string
}

// TurnEnded closes one turn of the agent.
type TurnEnded struct {
	// IsError is set when the turn ended in failure, including an
	// interrupt.
	IsError bool
	// Outcome is the harness's name for how the turn ended, such as
	// "success" or "error_during_execution".
	Outcome string
	// StopReason is the model's reason for stopping, such as "end_turn"
	// or "tool_use"; empty when the harness gives none.
	StopReason string
	// NumTurns is the harness's own count; its meaning is the harness's.
	NumTurns int
	Duration time.Duration
	// TotalCostUSD is what the task has cost so far, at the harness's
	// prices: a running total, not the cost of this turn.
	TotalCostUSD             float64
	InputTokens              int64
	OutputTokens             int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
}

// PermissionRequested is the harness asking whether it may run a tool.
type PermissionRequested struct {
	RequestID string
	Tool      string
	Input     json.RawMessage
}

// PauseAcknowledged carries the agent's note on where it stopped.
type PauseAcknowledged struct {
	Note string
}

// PauseSettled marks the pause taking effect. Interrupted is set when the
// daemon had to interrupt the agent to get there.
type PauseSettled struct {
	Interrupted bool
}

// QuotaObserved is the harness reporting the subscription's usage limits.
type QuotaObserved struct {
	Status  protocol.QuotaStatus
	Windows []protocol.QuotaWindow
}

type HarnessStarted struct {
	PID     int
	Model   string
	Workdir string
}

// HarnessExited ends the task's harness process. ExitCode is -1 when the
// process never started or was killed; Error then says why.
//
// CutShortBy is set when the turn was cut short and the task left paused,
// to be resumed by the owner: "restarted" when the daemon restarted after
// dying during the turn, "stopped" when it shut down during the turn, and
// "interrupted" when the owner interrupted the turn.
// NewSession is then set when no harness session was recorded, so that
// resuming starts a new one with the task's first prompt.
type HarnessExited struct {
	ExitCode   int
	Error      string
	Stderr     string
	CutShortBy string
	NewSession bool
}

// MessageSent is the agent sending Text to the task To through its
// inbox (docs/adr/2026-10-08-inbox-delivery.md). HandBack is set when
// the server sent the agent's final reply of a turn to its parent for it
// (docs/adr/2026-10-09-agents-and-placement.md).
type MessageSent struct {
	To       protocol.TaskID
	Text     string
	HandBack bool
}

// MessageReceived is a message delivered to the agent as a prompt. From
// names the task that sent it, and is nil for the server's notice that
// a child ended. HandBack is set when the message is a child's final
// reply of a turn, handed back to it as the child's report.
type MessageReceived struct {
	From     *protocol.TaskID
	Text     string
	HandBack bool
}

// ChildSpawned is the agent starting the task Child with Prompt.
type ChildSpawned struct {
	Child  protocol.TaskID
	Prompt string
}

// ChildEnded is the task Child, which the agent spawned, ending for good
// in State, the server's name for a stopped or failed task.
type ChildEnded struct {
	Child protocol.TaskID
	State string
}

// MessageUndeliverable is the task To, to which the agent sent messages,
// ending for good in State, the server's name for a stopped or failed
// task, before they were delivered. They never will be.
type MessageUndeliverable struct {
	To    protocol.TaskID
	State string
}

// TaskMoved is the task starting afresh on the daemon To, its earlier
// work having been lost with the daemon From, which the server declared
// lost (docs/adr/2026-10-08-daemon-loss.md). Prompt is what the new
// session starts with: the task's first prompt and a note saying so.
type TaskMoved struct {
	From   protocol.DaemonID
	To     protocol.DaemonID
	Prompt string
}

// BranchPushed is the daemon reporting, at the end of a turn, the branch
// the task's work is delivered on (docs/adr/2026-10-08-work-delivery.md),
// with the meaning of protocol.BranchPushed: Commit is the commit the
// branch holds, Ahead counts the branch's commits beyond the ref the
// task started from, and Uncommitted the files the workspace held
// uncommitted. With Error empty, the remote holds Commit when Ahead is
// above zero, and nothing was pushed when Ahead is zero. Error says why
// the branch could not be read or pushed.
type BranchPushed struct {
	Branch      string
	Commit      string
	Ahead       int
	Uncommitted int
	Error       string
}

// Unknown is a record no other Body describes, kept so that nothing is
// silently dropped. RecordKind is the event or command kind; Type is the
// harness's own type for a harness line, empty otherwise. Raw is the
// record's payload as stored.
type Unknown struct {
	RecordKind      string
	Type            string
	Raw             json.RawMessage
	ParentToolUseID string
}

// ParentToolUseID returns the ParentToolUseID of body, or "" for a body
// that has none: the entry belongs to the task's main conversation.
func ParentToolUseID(body Body) string {
	switch b := body.(type) {
	case AgentText:
		return b.ParentToolUseID
	case AgentThinking:
		return b.ParentToolUseID
	case ToolCall:
		return b.ParentToolUseID
	case ToolResult:
		return b.ParentToolUseID
	case Unknown:
		return b.ParentToolUseID
	}
	return ""
}

func (OwnerPrompt) Kind() Kind          { return KindOwnerPrompt }
func (PauseRequested) Kind() Kind       { return KindPauseRequested }
func (StopRequested) Kind() Kind        { return KindStopRequested }
func (Interrupted) Kind() Kind          { return KindInterrupted }
func (PermissionAnswered) Kind() Kind   { return KindPermissionAnswered }
func (AgentText) Kind() Kind            { return KindAgentText }
func (AgentThinking) Kind() Kind        { return KindAgentThinking }
func (ToolCall) Kind() Kind             { return KindToolCall }
func (ToolResult) Kind() Kind           { return KindToolResult }
func (TurnEnded) Kind() Kind            { return KindTurnEnded }
func (PermissionRequested) Kind() Kind  { return KindPermissionRequested }
func (PauseAcknowledged) Kind() Kind    { return KindPauseAcknowledged }
func (PauseSettled) Kind() Kind         { return KindPauseSettled }
func (QuotaObserved) Kind() Kind        { return KindQuotaObserved }
func (HarnessStarted) Kind() Kind       { return KindHarnessStarted }
func (HarnessExited) Kind() Kind        { return KindHarnessExited }
func (MessageSent) Kind() Kind          { return KindMessageSent }
func (MessageReceived) Kind() Kind      { return KindMessageReceived }
func (ChildSpawned) Kind() Kind         { return KindChildSpawned }
func (ChildEnded) Kind() Kind           { return KindChildEnded }
func (MessageUndeliverable) Kind() Kind { return KindMessageUndeliverable }
func (TaskMoved) Kind() Kind            { return KindTaskMoved }
func (BranchPushed) Kind() Kind         { return KindBranchPushed }
func (Unknown) Kind() Kind              { return KindUnknown }

func (OwnerPrompt) body()          {}
func (PauseRequested) body()       {}
func (StopRequested) body()        {}
func (Interrupted) body()          {}
func (PermissionAnswered) body()   {}
func (AgentText) body()            {}
func (AgentThinking) body()        {}
func (ToolCall) body()             {}
func (ToolResult) body()           {}
func (TurnEnded) body()            {}
func (PermissionRequested) body()  {}
func (PauseAcknowledged) body()    {}
func (PauseSettled) body()         {}
func (QuotaObserved) body()        {}
func (HarnessStarted) body()       {}
func (HarnessExited) body()        {}
func (MessageSent) body()          {}
func (MessageReceived) body()      {}
func (ChildSpawned) body()         {}
func (ChildEnded) body()           {}
func (MessageUndeliverable) body() {}
func (TaskMoved) body()            {}
func (BranchPushed) body()         {}
func (Unknown) body()              {}
