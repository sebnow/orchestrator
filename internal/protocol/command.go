package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
	// CommandWithdraw: Withdraw.
	CommandWithdraw CommandKind = "withdraw"
	// CommandDiscard tells the daemon that the owner dismissed the task,
	// which has no process and takes no more commands: the daemon deletes
	// what it keeps of the task, its record, journal and workspace. No
	// payload.
	CommandDiscard CommandKind = "discard"
	// CommandLogin asks the daemon to log its harness in
	// (docs/adr/2026-10-10-harness-login.md). It is the daemon's, not a
	// task's: its TaskID is empty. The daemon starts the harness's
	// login, ending one it runs already, and reports LoginStarted with
	// the URL to authorise at, then LoginFinished. No payload.
	CommandLogin CommandKind = "login"
	// CommandLoginCode: LoginCode, the daemon's, as CommandLogin.
	CommandLoginCode CommandKind = "login_code"
	// CommandHostKeys: HostKeys, the daemon's, as CommandLogin. The
	// server sends one when the daemon connects and whenever the keys
	// change; each replaces the daemon's earlier ones.
	CommandHostKeys CommandKind = "host_keys"
)

// Command is one entry in a daemon's command log. ID increases with every
// command the server issues and stays the same when the command is sent
// again. Epoch is the epoch of the server's database the command
// was issued in (docs/adr/2026-10-10-server-loss.md); a restored database
// issues under a new epoch, and may issue ids again in it. A daemon
// ignores a command whose epoch is that of the last command it applied
// and whose ID is at most that command's. TaskID is empty for a command
// to the daemon itself, of the kinds IsDaemons names.
type Command struct {
	ID       uint64          `json:"id"`
	Epoch    Epoch           `json:"epoch,omitempty"`
	DaemonID DaemonID        `json:"daemon_id"`
	TaskID   TaskID          `json:"task_id,omitempty"`
	Kind     CommandKind     `json:"kind"`
	Time     time.Time       `json:"time"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// StartTask starts a task on the daemon
// (docs/adr/2026-10-07-task-interface.md). Without a Workspace the daemon
// prepares an empty directory. An empty Model names no model.
//
// Tools names the gateway tools, of ToolSpawnTask and ToolSendMessage,
// that the task's agent may call
// (docs/adr/2026-10-09-agents-and-placement.md); the daemon's permission
// and pause tools are always available. A nil Tools, as in every start
// issued before the field existed, allows every gateway tool; an empty
// one allows none of them.
//
// Effort is one of Efforts, which the harness adapter maps to its own
// levels; empty leaves the harness's default
// (docs/adr/2026-10-10-agent-models-and-capacity.md).
//
// ToolClasses restricts the harness's own tools to those of the classes
// it names, of ToolClasses; nil or empty leaves every tool. The gateway
// tools are not harness tools and Tools alone governs them.
type StartTask struct {
	Prompt       string      `json:"prompt"`
	SystemPrompt string      `json:"system_prompt,omitempty"`
	Workspace    *Workspace  `json:"workspace,omitempty"`
	Model        string      `json:"model,omitempty"`
	Effort       string      `json:"effort,omitempty"`
	PauseLimits  PauseLimits `json:"pause_limits"`
	Tools        []string    `json:"tools,omitzero"`
	ToolClasses  []string    `json:"tool_classes,omitempty"`
}

// The neutral classes of harness tools an agent may be restricted to,
// which each harness adapter maps to its own tools.
const (
	// ToolClassRead reads and searches files.
	ToolClassRead = "read"
	// ToolClassEdit writes and edits files.
	ToolClassEdit = "edit"
	// ToolClassShell runs commands.
	ToolClassShell = "shell"
	// ToolClassWeb searches and fetches from the web.
	ToolClassWeb = "web"
	// ToolClassSubagents starts the harness's own subagents.
	ToolClassSubagents = "subagents"
	// ToolClassMCP is the tools of MCP servers other than the daemon's
	// gateway.
	ToolClassMCP = "mcp"
)

// ToolClasses are the classes StartTask.ToolClasses chooses among.
var ToolClasses = []string{ToolClassRead, ToolClassEdit, ToolClassShell, ToolClassWeb, ToolClassSubagents, ToolClassMCP}

// The neutral effort scale an agent and a task's start choose from.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
	EffortMax    = "max"
)

// Efforts are the efforts StartTask.Effort may name, lowest first.
var Efforts = []string{EffortLow, EffortMedium, EffortHigh, EffortMax}

// The gateway tools a task's agent may be allowed, by the names the
// agent calls them.
const (
	ToolSpawnTask   = "spawn_task"
	ToolSendMessage = "send_message"
)

// AgentTools are the gateway tools StartTask.Tools chooses among.
var AgentTools = []string{ToolSpawnTask, ToolSendMessage}

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

// Prompt is a follow-up prompt to a task. From is nil for the owner's
// prompt; it is set when the prompt delivers messages from the inbox
// (docs/adr/2026-10-08-inbox-delivery.md), and names the task the oldest
// of them came from or, for the server's notice that a child ended, that
// child. The server words Text, which names every sender.
//
// A prompt to a task whose turn runs waits for the turn to end: the
// daemon holds it, reporting PromptHeld, and sends it as the next turn's
// prompt, reporting PromptReleased. Steer, which only the owner sets,
// sends it now instead: the daemon interrupts the running turn and sends
// Text as the next prompt in the same session. To a task with no turn
// running, either starts the next turn.
type Prompt struct {
	Text  string  `json:"text"`
	From  *TaskID `json:"from,omitempty"`
	Steer bool    `json:"steer,omitempty"`
}

// Withdraw asks the daemon to drop the prompt command Prompt, which it
// holds until the running turn ends, before sending it to the harness.
// A prompt the daemon no longer holds is not withdrawn.
type Withdraw struct {
	Prompt uint64 `json:"prompt"`
}

// AnswerPermission answers the PermissionRequested event that carried
// RequestID. Message tells the agent why when the request is denied.
type AnswerPermission struct {
	RequestID string `json:"request_id"`
	Allow     bool   `json:"allow"`
	Message   string `json:"message,omitempty"`
}

// IsDaemons reports whether commands of kind are to the daemon itself
// rather than to one of its tasks.
func (kind CommandKind) IsDaemons() bool {
	return kind == CommandLogin || kind == CommandLoginCode || kind == CommandHostKeys
}

// HostKeys are the forges' ssh host keys the server distributes: each
// of Lines is a known_hosts line, "host keytype key". The daemon writes
// them to a known_hosts file of its own, replacing what it held, and its
// ssh checks a remote's host key against that file.
type HostKeys struct {
	Lines []string `json:"lines"`
}

// LoginCode is the code the owner copied from the page the
// authorisation URL led to, for the login that the CommandLogin with id
// Login started. The daemon gives it to the harness's login, or reports
// LoginFinished with an error when that login no longer waits for one.
type LoginCode struct {
	Login uint64 `json:"login"`
	Code  string `json:"code"`
}

// Position is where command is in the server's command log.
func (c Command) Position() CommandPosition {
	return CommandPosition{Epoch: c.Epoch, ID: c.ID}
}

// Epoch names one of a server database's epochs: the database's
// first, or one started by a restore (docs/adr/2026-10-10-server-loss.md).
type Epoch string

// ParseEpoch returns raw as an Epoch: 1 to 64 ASCII letters and digits.
func ParseEpoch(raw string) (Epoch, error) {
	if raw == "" || len(raw) > 64 {
		return "", fmt.Errorf("epoch %q is not 1 to 64 letters and digits", raw)
	}
	for _, c := range []byte(raw) {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z') {
			return "", fmt.Errorf("epoch %q is not 1 to 64 letters and digits", raw)
		}
	}
	return Epoch(raw), nil
}

// CommandPosition is a command's place in the server's command log, the
// epoch it was issued in and its id. The command stream sends it as each
// command's event id, EPOCH:ID, and the daemon sends the position of the
// last command it applied as Last-Event-ID. A position without an Epoch
// is one a daemon recorded before commands had epochs, and is written as
// the bare ID.
type CommandPosition struct {
	Epoch Epoch
	ID    uint64
}

func (p CommandPosition) String() string {
	if p.Epoch == "" {
		return strconv.FormatUint(p.ID, 10)
	}
	return string(p.Epoch) + ":" + strconv.FormatUint(p.ID, 10)
}

// ParseCommandPosition parses EPOCH:ID, or a bare ID, which has no Epoch.
func ParseCommandPosition(raw string) (CommandPosition, error) {
	rawEpoch, rawID, hasEpoch := strings.Cut(raw, ":")
	if !hasEpoch {
		rawID = rawEpoch
	}
	id, err := strconv.ParseUint(rawID, 10, 63)
	if err != nil {
		return CommandPosition{}, errors.New("command position " + strconv.Quote(raw) + " is not EPOCH:ID")
	}
	if !hasEpoch {
		return CommandPosition{ID: id}, nil
	}
	epoch, err := ParseEpoch(rawEpoch)
	if err != nil {
		return CommandPosition{}, fmt.Errorf("command position %q: %w", raw, err)
	}
	return CommandPosition{Epoch: epoch, ID: id}, nil
}
