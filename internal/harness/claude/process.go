package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// Name identifies Claude Code in event envelopes.
const Name = "claude-code"

// gatewayServer is the name the daemon's gateway has in --mcp-config, so
// its tools reach the model as mcp__orchestrator__<tool>.
const gatewayServer = "orchestrator"

// gatewayTimeoutMS is the per-server tool timeout given to the gateway.
// For an HTTP MCP server Claude Code otherwise abandons a request with no
// response byte after 60 s and an idle call after 5 minutes; the
// permission tool must wait as long as the owner takes to answer. The
// value is the documented default of MCP_TOOL_TIMEOUT, about 28 hours.
const gatewayTimeoutMS = 100_000_000

const stderrTailBytes = 8 << 10

// parentSessionEnv names variables a Claude Code session sets in the
// processes it spawns. A daemon started from such a session must not pass
// them on: the harness it starts is a session of its own.
var parentSessionEnv = []string{
	"CLAUDECODE",
	"CLAUDE_CODE_AGENT",
	"CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_ENTRYPOINT",
	"CLAUDE_CODE_EXECPATH",
	"CLAUDE_CODE_MESSAGING_SOCKET",
	"CLAUDE_CODE_MESSAGING_TOKEN",
	"CLAUDE_CODE_SESSION_ATTENDED",
	"CLAUDE_CODE_SESSION_ID",
	"CLAUDE_EFFORT",
	"CLAUDE_PID",
}

// crossSessionTools are Claude Code's built-in tools that find and
// message other Claude sessions on the same machine. A live run showed a
// task loading them and messaging the owner's interactive session.
var crossSessionTools = []string{"SendMessage", "ListAgents"}

// Harness runs the Claude Code CLI.
type Harness struct {
	path    string
	version string
}

// New finds the version of the claude executable at path.
func New(ctx context.Context, path string) (*Harness, error) {
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = childEnv(os.Environ())
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("run %s --version: %w", path, err)
	}
	// The output reads "2.1.289 (Claude Code)".
	version, _, _ := strings.Cut(strings.TrimSpace(string(out)), " ")
	if version == "" {
		return nil, fmt.Errorf("run %s --version: no version in %q", path, out)
	}
	return &Harness{path: path, version: version}, nil
}

func (h *Harness) Info() protocol.Harness {
	return protocol.Harness{Name: Name, Version: h.version}
}

func (h *Harness) Start(ctx context.Context, spec harness.Spec) (harness.Process, error) {
	args, err := arguments(spec)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, h.path, args...)
	cmd.Dir = spec.Workdir
	cmd.Env = childEnv(os.Environ())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &tail{limit: stderrTailBytes}
	cmd.Stderr = stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", h.path, err)
	}
	return &process{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: stderr}, nil
}

func arguments(spec harness.Spec) ([]string, error) {
	config, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			gatewayServer: map[string]any{
				"type":    "http",
				"url":     spec.Gateway.URL,
				"timeout": gatewayTimeoutMS,
			},
		},
	})
	if err != nil {
		return nil, err
	}
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", spec.Model,
		// Every tool call that needs approval goes to the gateway, whatever
		// the machine's default permission mode is.
		"--permission-mode", "default",
		// Keep the machine owner's user settings, plugins and MCP servers
		// out of the task.
		"--setting-sources", "project",
		"--strict-mcp-config",
		"--mcp-config", string(config),
		"--permission-prompt-tool", gatewayTool(spec.Gateway.PermissionTool),
		// Claude Code's own messaging reaches other Claude sessions on the
		// machine, not other tasks; a task must use the gateway's tools
		// (docs/adr/2026-10-08-inbox-delivery.md, Consequences).
		"--disallowedTools", strings.Join(crossSessionTools, ","),
	}
	if spec.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", spec.SystemPrompt)
	}
	if spec.Resume != "" {
		// The session keeps the system prompt it recorded first; passing
		// one again is harmless (docs/design/2026-10-08-resume-spike.md).
		args = append(args, "--resume", spec.Resume)
	}
	if len(spec.Gateway.Tools) > 0 {
		args = append(args, "--allowedTools")
		for _, tool := range spec.Gateway.Tools {
			args = append(args, gatewayTool(tool))
		}
	}
	return args, nil
}

func gatewayTool(name string) string {
	return "mcp__" + gatewayServer + "__" + name
}

func childEnv(env []string) []string {
	return slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(parentSessionEnv, name)
	})
}

type permissionArguments struct {
	ToolName string          `json:"tool_name"`
	Input    json.RawMessage `json:"input"`
}

// ParsePermission decodes the arguments Claude Code passes to the
// --permission-prompt-tool: tool_name, input and tool_use_id.
func (h *Harness) ParsePermission(arguments json.RawMessage) (harness.PermissionRequest, error) {
	var args permissionArguments
	if err := json.Unmarshal(arguments, &args); err != nil {
		return harness.PermissionRequest{}, fmt.Errorf("permission arguments: %w", err)
	}
	if args.ToolName == "" {
		return harness.PermissionRequest{}, errors.New("permission arguments: no tool_name")
	}
	if len(args.Input) == 0 || string(args.Input) == "null" {
		args.Input = json.RawMessage(`{}`)
	}
	return harness.PermissionRequest{Tool: args.ToolName, Input: args.Input}, nil
}

// EncodeDecision returns the reply text of the --permission-prompt-tool:
// a PermissionResult, as the SDK reference defines it, encoded as JSON
// text content. The JSON-in-text wrapping is undocumented; it is what the
// spike's perm_mcp.py returned to Claude Code 2.1.289.
func (h *Harness) EncodeDecision(req harness.PermissionRequest, decision harness.Decision) string {
	var reply any
	if decision.Allow {
		reply = struct {
			Behavior     string          `json:"behavior"`
			UpdatedInput json.RawMessage `json:"updatedInput"`
		}{"allow", req.Input}
	} else {
		message := decision.Message
		if message == "" {
			message = "Denied by the orchestrator."
		}
		reply = struct {
			Behavior string `json:"behavior"`
			Message  string `json:"message"`
		}{"deny", message}
	}
	text, err := json.Marshal(reply)
	if err != nil {
		// req.Input came from a decoded JSON document.
		panic(err)
	}
	return string(text)
}

type process struct {
	cmd    *exec.Cmd
	stdout *bufio.Reader
	stderr *tail

	stdinMu    sync.Mutex
	stdin      io.WriteCloser
	interrupts int
}

func (p *process) PID() int {
	return p.cmd.Process.Pid
}

func (p *process) Read() (harness.Output, error) {
	for {
		line, err := p.stdout.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			return classify(line), nil
		}
		if err != nil {
			return harness.Output{}, err
		}
	}
}

// classify keeps a line that does not parse as opaque output: it is still
// the harness's record, and the server sees the same line.
func classify(line []byte) harness.Output {
	out := harness.Output{Line: line}
	msg, err := Parse(line)
	if err != nil {
		return out
	}
	_, out.TurnEnded = msg.Result()
	if _, isInit := msg.Init(); isInit || out.TurnEnded {
		out.SessionID = msg.SessionID
	}
	out.Answering = msg.Answering()
	if info, ok := msg.RateLimit(); ok {
		quota := quotaObserved(info)
		out.Quota = &quota
	}
	return out
}

func quotaObserved(info RateLimitInfo) protocol.QuotaObserved {
	status := protocol.QuotaUnknown
	switch info.Status {
	case "allowed":
		status = protocol.QuotaAllowed
	case "allowed_warning":
		status = protocol.QuotaWarning
	case "rejected":
		status = protocol.QuotaRejected
	}
	quota := protocol.QuotaObserved{Status: status, Windows: []protocol.QuotaWindow{}}
	for name, window := range info.UnifiedWindows {
		quota.Windows = append(quota.Windows, protocol.QuotaWindow{
			Name:        name,
			Utilization: window.Utilization,
			ResetsAt:    time.Unix(window.ResetsAt, 0).UTC(),
		})
	}
	if len(quota.Windows) == 0 && info.Utilization != nil {
		quota.Windows = append(quota.Windows, protocol.QuotaWindow{
			Name:        info.RateLimitType,
			Utilization: *info.Utilization,
			ResetsAt:    time.Unix(info.ResetsAt, 0).UTC(),
		})
	}
	slices.SortFunc(quota.Windows, func(a, b protocol.QuotaWindow) int { return strings.Compare(a.Name, b.Name) })
	return quota
}

func (p *process) Prompt(id, text string) error {
	return p.write(EncodeUserMessage(text, id))
}

func (p *process) Interrupt() error {
	p.stdinMu.Lock()
	p.interrupts++
	id := "interrupt-" + strconv.Itoa(p.interrupts)
	p.stdinMu.Unlock()
	return p.write(EncodeInterrupt(id))
}

func (p *process) write(line []byte) error {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	_, err := p.stdin.Write(line)
	return err
}

func (p *process) CloseInput() error {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()
	return p.stdin.Close()
}

func (p *process) Kill() error {
	return p.cmd.Process.Kill()
}

func (p *process) Wait() protocol.HarnessExited {
	err := p.cmd.Wait()
	exited := protocol.HarnessExited{ExitCode: p.cmd.ProcessState.ExitCode(), Stderr: p.stderr.String()}
	if _, isExit := errors.AsType[*exec.ExitError](err); err != nil && !isExit {
		exited.Error = err.Error()
	} else if exited.ExitCode == -1 {
		exited.Error = p.cmd.ProcessState.String()
	}
	return exited
}

// tail keeps the last limit bytes written to it.
type tail struct {
	mu    sync.Mutex
	limit int
	buf   []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - t.limit; over > 0 {
		t.buf = slices.Delete(t.buf, 0, over)
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
