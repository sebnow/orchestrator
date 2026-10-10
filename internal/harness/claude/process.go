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
	"syscall"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/runas"
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
	return NewAs(ctx, path, runas.User{})
}

// NewAs finds the version of the claude executable at path, running it
// as user, so that a harness user who cannot run it fails here rather
// than at a task's start.
func NewAs(ctx context.Context, path string, user runas.User) (*Harness, error) {
	cmd := user.Command(ctx, "", path, []string{"--version"}, childEnv(os.Environ()), nil)
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
	promptFile, err := writeSystemPrompt(spec)
	if err != nil {
		return nil, err
	}
	removePrompt := func() {
		if promptFile != "" {
			os.Remove(promptFile)
		}
	}
	proc, err := h.startProcess(ctx, spec, promptFile)
	if err != nil {
		removePrompt()
		return nil, err
	}
	proc.cleanup = removePrompt
	return proc, nil
}

func (h *Harness) startProcess(ctx context.Context, spec harness.Spec, promptFile string) (*process, error) {
	cmd, err := h.command(ctx, spec, promptFile)
	if err != nil {
		return nil, err
	}
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
	return &process{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: stderr, viaSudo: spec.RunAs.Other()}, nil
}

// writeSystemPrompt writes spec's system prompt to a file in spec.FileDir
// and returns its path, or "" when there is no prompt. The file keeps the
// prompt off the command line, which every local user can read in the
// process list. Only the daemon's user may read it, unless the harness
// runs as another user, who must read it too.
func writeSystemPrompt(spec harness.Spec) (string, error) {
	if spec.SystemPrompt == "" {
		return "", nil
	}
	f, err := os.CreateTemp(spec.FileDir, "system-prompt-*.txt")
	if err != nil {
		return "", fmt.Errorf("write the system prompt: %w", err)
	}
	_, err = f.WriteString(spec.SystemPrompt)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && spec.RunAs.Other() {
		err = os.Chmod(f.Name(), 0o644)
	}
	if err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("write the system prompt: %w", err)
	}
	return f.Name(), nil
}

// command returns the command that runs the harness spec describes, with
// the system prompt read from promptFile, if not empty.
func (h *Harness) command(ctx context.Context, spec harness.Spec, promptFile string) (*exec.Cmd, error) {
	args, err := arguments(spec, promptFile)
	if err != nil {
		return nil, err
	}
	return spec.RunAs.Command(ctx, spec.Workdir, h.path, args, childEnv(os.Environ()), nil), nil
}

func arguments(spec harness.Spec, promptFile string) ([]string, error) {
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
	settings, err := remoteSubagentSettings()
	if err != nil {
		return nil, err
	}
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		// The text and thinking of the harness's own subagents reach the
		// transcript too, nested under the tool call that started each
		// (https://code.claude.com/docs/en/headless.md, "Follow subagent
		// messages"; Claude Code 2.1.211 or later).
		"--forward-subagent-text",
		"--model", spec.Model,
		// Every tool call that needs approval goes to the gateway, whatever
		// the machine's default permission mode is.
		"--permission-mode", "default",
		// Keep the machine owner's user settings, plugins and MCP servers
		// out of the task.
		"--setting-sources", "project",
		// Send remote subagent calls to the gateway. Claude Code 2.1.289
		// applied these flag settings alongside --setting-sources project
		// (docs/design/2026-10-09-remote-subagents.md).
		"--settings", settings,
		"--strict-mcp-config",
		"--mcp-config", string(config),
		"--permission-prompt-tool", gatewayTool(spec.Gateway.PermissionTool),
		// Claude Code's own messaging reaches other Claude sessions on the
		// machine, not other tasks; a task must use the gateway's tools
		// (docs/adr/2026-10-08-inbox-delivery.md, Consequences).
		"--disallowedTools", strings.Join(crossSessionTools, ","),
	}
	if spec.Effort != "" {
		effort, ok := efforts[spec.Effort]
		if !ok {
			return nil, fmt.Errorf("effort %q is not one of %s", spec.Effort, strings.Join(protocol.Efforts, ", "))
		}
		args = append(args, "--effort", effort)
	}
	if promptFile != "" {
		args = append(args, "--append-system-prompt-file", promptFile)
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

// efforts maps the neutral effort scale to the levels of --effort, which
// Claude Code 2.1.289 lists as low, medium, high, xhigh and max; xhigh
// has no neutral level.
var efforts = map[string]string{
	protocol.EffortLow:    "low",
	protocol.EffortMedium: "medium",
	protocol.EffortHigh:   "high",
	protocol.EffortMax:    "max",
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
	// viaSudo is set when sudo runs the harness as another user; the
	// process is then sudo's.
	viaSudo bool
	// cleanup deletes the files written for the process, once it exits.
	cleanup func()

	stdinMu    sync.Mutex
	stdin      io.WriteCloser
	interrupts int

	// turn is touched only by Read.
	turn turn
}

func (p *process) PID() int {
	return p.cmd.Process.Pid
}

func (p *process) Read() (harness.Output, error) {
	for {
		line, err := p.stdout.ReadBytes('\n')
		line = bytes.TrimRight(line, "\r\n")
		if len(line) > 0 {
			return p.turn.classify(line), nil
		}
		if err != nil {
			return harness.Output{}, err
		}
	}
}

// turn decides which result ends a turn as the daemon sees it.
//
// Claude Code writes a result when the model stops, even while a
// subagent it started runs on in the background; when the subagent
// finishes, Claude Code starts a turn of its own to report it, and
// writes another result (docs/design/2026-10-09-subagent-stream.md). The
// turn ends at the first result written while no subagent is running
// (docs/design/2026-10-09-background-subagent-turn.md).
type turn struct {
	// subagents holds the task_id of each subagent reported started and
	// not yet reported finished.
	subagents map[string]bool
	// answering collects the prompt ids echoed by results that did not
	// end the turn, for the result that does.
	answering []string
}

// classify keeps a line that does not parse as opaque output: it is still
// the harness's record, and the server sees the same line.
func (t *turn) classify(line []byte) harness.Output {
	out := harness.Output{Line: line}
	msg, err := Parse(line)
	if err != nil {
		return out
	}
	t.track(msg)
	_, isInit := msg.Init()
	_, isResult := msg.Result()
	if isInit || isResult {
		out.SessionID = msg.SessionID
	}
	out.Answering = msg.Answering()
	if isResult {
		if len(t.subagents) > 0 {
			t.answering = append(t.answering, out.Answering...)
		} else {
			out.TurnEnded = true
			out.Answering = union(t.answering, out.Answering)
			t.answering = nil
		}
	}
	if info, ok := msg.RateLimit(); ok {
		quota := quotaObserved(info)
		out.Quota = &quota
	}
	return out
}

// track follows the subagents running in the process. Only subagents
// count: a background Bash command or Monitor watch may never end, and
// Claude Code killed a background Bash command itself once its input was
// closed (docs/design/2026-10-07-mod-vs-stdout-spike.md). Each signal
// that a subagent finished is enough, since a release may not send all
// of them.
func (t *turn) track(msg Message) {
	if started, ok := msg.TaskStarted(); ok && started.TaskType == TaskTypeLocalAgent && !started.Ambient && started.TaskID != "" {
		if t.subagents == nil {
			t.subagents = map[string]bool{}
		}
		t.subagents[started.TaskID] = true
	}
	if done, ok := msg.TaskNotification(); ok {
		delete(t.subagents, done.TaskID)
	}
	if updated, ok := msg.TaskUpdated(); ok && updated.Ended() {
		delete(t.subagents, updated.TaskID)
	}
	if changed, ok := msg.BackgroundTasksChanged(); ok && len(changed.Tasks) == 0 {
		clear(t.subagents)
	}
}

// union returns the ids of a followed by those of b not in a.
func union(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	out := slices.Clone(a)
	for _, id := range b {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
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
	if !p.viaSudo {
		return p.cmd.Process.Kill()
	}
	// sudo relays SIGTERM to the harness but not SIGKILL, and the daemon
	// may not signal another user's process. Closed input ends the
	// harness too, should it outlast SIGTERM.
	err := p.cmd.Process.Signal(syscall.SIGTERM)
	p.CloseInput()
	return err
}

func (p *process) Wait() protocol.HarnessExited {
	err := p.cmd.Wait()
	p.cleanup()
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
