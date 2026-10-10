// Package harness is the harness-neutral contract between the daemon and
// an agent harness such as Claude Code
// (docs/adr/2026-10-07-harness-independence.md). An implementation turns
// its harness's own formats into these types; the daemon sees nothing
// else.
package harness

import (
	"context"
	"encoding/json"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/runas"
)

// Harness starts harness processes and translates the harness's side of
// the gateway's permission tool.
type Harness interface {
	Info() protocol.Harness
	Start(ctx context.Context, spec Spec) (Process, error)
	// ParsePermission decodes the arguments the harness passed to the
	// gateway's permission tool.
	ParsePermission(arguments json.RawMessage) (PermissionRequest, error)
	// EncodeDecision returns the permission tool's reply text that tells
	// the harness the decision on req.
	EncodeDecision(req PermissionRequest, decision Decision) string
}

// Spec describes one harness process.
type Spec struct {
	Workdir string
	Model   string
	// Effort is one of protocol.Efforts, which the harness maps to its own
	// levels; empty leaves the harness's default.
	Effort string
	// SystemPrompt is added to the harness's own; empty adds nothing.
	SystemPrompt string
	// Resume names the harness session to continue, as reported in
	// Output.SessionID by an earlier process of the task; empty starts a
	// new session.
	Resume  string
	Gateway Gateway
	// RunAs is the OS user the process runs as; the zero User is the
	// daemon's own (docs/adr/2026-10-08-harness-user.md).
	RunAs runas.User
	// FileDir is where the harness may write files its process reads,
	// such as the system prompt, deleting them when the process exits.
	// RunAs must be able to read files there; empty means os.TempDir.
	FileDir string
}

// Gateway is the daemon's MCP endpoint for one task.
type Gateway struct {
	// URL serves MCP over streamable HTTP.
	URL string
	// PermissionTool is the tool the harness must call to ask for
	// permission to run a tool.
	PermissionTool string
	// Tools are gateway tools the agent may call without asking.
	Tools []string
}

// Process is one running harness. It lives for one turn of its task: the
// daemon closes its input once the turn has ended and nothing it sent is
// outstanding (docs/adr/2026-10-08-task-lifetime.md). Read, Wait and the
// writing methods may be called from different goroutines; the writing
// methods are safe for concurrent use with each other.
type Process interface {
	PID() int
	// Read returns the next line of output and io.EOF after the last.
	Read() (Output, error)
	// Prompt sends text as a user prompt. A turn that answers it lists id
	// in Output.Answering, when the harness supports echoing.
	Prompt(id, text string) error
	// Interrupt stops the running turn; the session stays alive.
	Interrupt() error
	// CloseInput tells the harness no more input will come, which ends it.
	CloseInput() error
	// Kill ends the process at once. A process run as another user is
	// sent SIGTERM and has its input closed instead, since the daemon
	// cannot kill it (docs/adr/2026-10-08-harness-user.md).
	Kill() error
	// Wait reaps the process once Read has returned io.EOF.
	Wait() protocol.HarnessExited
}

// Output is one line the harness wrote, with what the daemon needs to know
// about it. Only Line is always set.
type Output struct {
	Line []byte
	// TurnEnded marks the line that ends a turn: the harness does nothing
	// more until it is sent input. While work the turn started runs on in
	// the background, the turn has not ended, whatever the harness
	// reports in between.
	TurnEnded bool
	// Answering lists the ids of the prompts this line shows the harness
	// answering. A line that ends a turn lists every prompt the turn
	// answered.
	Answering []string
	// SessionID names the harness session the line reports, when it
	// reports one; Spec.Resume continues that session in a new process.
	SessionID string
	Quota     *protocol.QuotaObserved
}

type PermissionRequest struct {
	Tool  string
	Input json.RawMessage
}

// Decision answers a PermissionRequest. Message tells the agent why a
// request was denied.
type Decision struct {
	Allow   bool
	Message string
}
