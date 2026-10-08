package claude_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// TestMain lets the test binary stand in for the claude executable: with
// FAKE_CLAUDE=1 it plays a minimal Claude Code on stdin and stdout.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_CLAUDE") == "1" {
		fakeClaude()
		return
	}
	os.Exit(m.Run())
}

func fakeClaude() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("2.1.289 (Claude Code)")
		os.Exit(0)
	}
	out := json.NewEncoder(os.Stdout)
	cwd, _ := os.Getwd()
	_, inherited := os.LookupEnv("CLAUDECODE")
	out.Encode(map[string]any{
		"type": "system", "subtype": "init", "session_id": "fake-session", "model": "fake-model",
		"cwd": cwd, "argv": os.Args[1:], "inherited_claudecode": inherited,
	})
	fmt.Println("this line is not JSON")
	out.Encode(map[string]any{
		"type": "rate_limit_event", "session_id": "fake-session",
		"rate_limit_info": map[string]any{
			"status": "allowed", "resetsAt": 1791375000, "rateLimitType": "five_hour",
			"unifiedWindows": map[string]any{
				"seven_day": map[string]any{"utilization": 0.07, "resetsAt": 1791651600},
				"five_hour": map[string]any{"utilization": 0.29, "resetsAt": 1791375000},
			},
		},
	})
	out.Encode(map[string]any{
		"type": "rate_limit_event", "session_id": "fake-session",
		"rate_limit_info": map[string]any{"status": "allowed_warning", "resetsAt": 1791651600, "utilization": 0.8, "rateLimitType": "seven_day"},
	})
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var msg struct {
			Type      string `json:"type"`
			UUID      string `json:"uuid"`
			RequestID string `json:"request_id"`
			Message   struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(in.Bytes(), &msg); err != nil {
			fmt.Fprintln(os.Stderr, "fake claude: bad input:", err)
			os.Exit(2)
		}
		switch msg.Type {
		case "user":
			result := map[string]any{"type": "result", "subtype": "success", "session_id": "fake-session", "result": msg.Message.Content, "num_turns": 1}
			if msg.UUID != "" {
				result["user_message_uuid"] = msg.UUID
			}
			out.Encode(result)
		case "control_request":
			out.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": msg.RequestID}})
		}
	}
	fmt.Fprint(os.Stderr, "fake claude: stdin closed")
	os.Exit(3)
}

func fakeHarness(t *testing.T) *claude.Harness {
	t.Helper()
	t.Setenv("FAKE_CLAUDE", "1")
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := claude.New(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

var testSpec = harness.Spec{
	Model:        "haiku",
	SystemPrompt: "You work for the orchestrator.",
	Gateway: harness.Gateway{
		URL:            "http://127.0.0.1:4242/tasks/t-1/mcp",
		PermissionTool: "permission",
		Tools:          []string{"acknowledge_pause"},
	},
}

type fakeInit struct {
	Cwd                 string   `json:"cwd"`
	Argv                []string `json:"argv"`
	InheritedClaudeCode bool     `json:"inherited_claudecode"`
}

// startFake starts the fake and reads up to and including its two
// rate_limit_events.
func startFake(t *testing.T, spec harness.Spec) (harness.Process, fakeInit, []harness.Output) {
	t.Helper()
	h := fakeHarness(t)
	if spec.Workdir == "" {
		spec.Workdir = t.TempDir()
	}
	proc, err := h.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proc.Kill() })
	var init fakeInit
	var outputs []harness.Output
	for range 4 {
		out := read(t, proc)
		outputs = append(outputs, out)
	}
	if err := json.Unmarshal(outputs[0].Line, &init); err != nil {
		t.Fatalf("first line %s: %v", outputs[0].Line, err)
	}
	return proc, init, outputs
}

func read(t *testing.T, proc harness.Process) harness.Output {
	t.Helper()
	out, err := proc.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return out
}

func argValue(argv []string, flag string) (string, bool) {
	idx := slices.Index(argv, flag)
	if idx < 0 || idx+1 >= len(argv) {
		return "", false
	}
	return argv[idx+1], true
}

func TestGivenClaudeExecutableWhenCreatingHarnessThenInfoCarriesItsVersion(t *testing.T) {
	h := fakeHarness(t)

	got := h.Info()

	want := protocol.Harness{Name: "claude-code", Version: "2.1.289"}
	if got != want {
		t.Errorf("Info() = %+v, want %+v", got, want)
	}
}

func TestGivenSpecWhenStartingThenClaudeRunsInStreamJSONModeWiredToTheGateway(t *testing.T) {
	workdir := t.TempDir()
	spec := testSpec
	spec.Workdir = workdir

	_, init, _ := startFake(t, spec)

	argv := init.Argv
	for _, flag := range []string{"-p", "--verbose", "--strict-mcp-config"} {
		if !slices.Contains(argv, flag) {
			t.Errorf("argv lacks %s: %q", flag, argv)
		}
	}
	wantValues := map[string]string{
		"--input-format":           "stream-json",
		"--output-format":          "stream-json",
		"--model":                  "haiku",
		"--permission-mode":        "default",
		"--setting-sources":        "project",
		"--permission-prompt-tool": "mcp__orchestrator__permission",
		"--append-system-prompt":   "You work for the orchestrator.",
		"--allowedTools":           "mcp__orchestrator__acknowledge_pause",
	}
	for flag, want := range wantValues {
		if got, _ := argValue(argv, flag); got != want {
			t.Errorf("%s = %q, want %q", flag, got, want)
		}
	}
	raw, _ := argValue(argv, "--mcp-config")
	var config struct {
		MCPServers map[string]struct {
			Type    string `json:"type"`
			URL     string `json:"url"`
			Timeout int64  `json:"timeout"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatalf("--mcp-config %q: %v", raw, err)
	}
	server := config.MCPServers["orchestrator"]
	if server.Type != "http" || server.URL != spec.Gateway.URL || server.Timeout < int64(time.Hour/time.Millisecond) {
		t.Errorf("gateway server = %+v", server)
	}
	wantDir, _ := filepath.EvalSymlinks(workdir)
	gotDir, _ := filepath.EvalSymlinks(init.Cwd)
	if gotDir != wantDir {
		t.Errorf("cwd = %q, want %q", gotDir, wantDir)
	}
}

func TestGivenNoSystemPromptWhenStartingThenNoneIsAppended(t *testing.T) {
	spec := testSpec
	spec.SystemPrompt = ""

	_, init, _ := startFake(t, spec)

	if slices.Contains(init.Argv, "--append-system-prompt") {
		t.Errorf("argv = %q", init.Argv)
	}
}

func TestGivenDaemonInsideAClaudeCodeSessionWhenStartingThenTheSessionMarkersAreNotInherited(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")

	_, init, _ := startFake(t, testSpec)

	if init.InheritedClaudeCode {
		t.Error("harness inherited CLAUDECODE")
	}
}

func TestGivenOutputLinesWhenReadingThenEachIsKeptAndClassified(t *testing.T) {
	_, _, outputs := startFake(t, testSpec)

	if string(outputs[1].Line) != "this line is not JSON" || outputs[1].TurnEnded || outputs[1].Quota != nil {
		t.Errorf("opaque line = %+v", outputs[1])
	}
	quota := outputs[2].Quota
	if quota == nil {
		t.Fatalf("rate_limit_event not classified: %s", outputs[2].Line)
	}
	want := protocol.QuotaObserved{Status: protocol.QuotaAllowed, Windows: []protocol.QuotaWindow{
		{Name: "five_hour", Utilization: 0.29, ResetsAt: time.Unix(1791375000, 0).UTC()},
		{Name: "seven_day", Utilization: 0.07, ResetsAt: time.Unix(1791651600, 0).UTC()},
	}}
	if quota.Status != want.Status || !slices.Equal(quota.Windows, want.Windows) {
		t.Errorf("quota = %+v, want %+v", *quota, want)
	}
}

func TestGivenDocumentedRateLimitShapeWhenReadingThenItBecomesOneWindow(t *testing.T) {
	_, _, outputs := startFake(t, testSpec)

	quota := outputs[3].Quota
	if quota == nil {
		t.Fatalf("rate_limit_event not classified: %s", outputs[3].Line)
	}
	want := []protocol.QuotaWindow{{Name: "seven_day", Utilization: 0.8, ResetsAt: time.Unix(1791651600, 0).UTC()}}
	if quota.Status != protocol.QuotaWarning || !slices.Equal(quota.Windows, want) {
		t.Errorf("quota = %+v", *quota)
	}
}

func TestGivenPromptWithIDWhenTheTurnEndsThenOutputMarksTheEndAndEchoesTheID(t *testing.T) {
	proc, _, _ := startFake(t, testSpec)

	if err := proc.Prompt("prompt-1", "hello"); err != nil {
		t.Fatal(err)
	}
	out := read(t, proc)

	if !out.TurnEnded || !slices.Equal(out.Answering, []string{"prompt-1"}) {
		t.Errorf("output = %+v, line %s", out, out.Line)
	}
}

func TestGivenRunningProcessWhenInterruptingThenClaudeReceivesAControlRequest(t *testing.T) {
	proc, _, _ := startFake(t, testSpec)

	if err := proc.Interrupt(); err != nil {
		t.Fatal(err)
	}
	out := read(t, proc)

	if !strings.Contains(string(out.Line), `"request_id":"interrupt-1"`) || out.TurnEnded {
		t.Errorf("output = %+v, line %s", out, out.Line)
	}
}

func TestGivenClosedInputWhenTheProcessExitsThenWaitReportsCodeAndStderr(t *testing.T) {
	proc, _, _ := startFake(t, testSpec)

	if err := proc.CloseInput(); err != nil {
		t.Fatal(err)
	}
	if _, err := proc.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("read after close = %v, want EOF", err)
	}
	exited := proc.Wait()

	if exited.ExitCode != 3 || exited.Stderr != "fake claude: stdin closed" || exited.Error != "" {
		t.Errorf("exited = %+v", exited)
	}
}

func TestGivenKilledProcessWhenWaitingThenExitCodeIsMinusOneWithTheSignal(t *testing.T) {
	proc, _, _ := startFake(t, testSpec)

	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := proc.Read(); err != nil {
			break
		}
	}
	exited := proc.Wait()

	if exited.ExitCode != -1 || !strings.Contains(exited.Error, "killed") {
		t.Errorf("exited = %+v", exited)
	}
}

func TestGivenPermissionToolArgumentsWhenParsingThenToolAndInputAreKept(t *testing.T) {
	h := fakeHarness(t)

	got, err := h.ParsePermission(json.RawMessage(`{"tool_name":"Bash","input":{"command":"touch a"},"tool_use_id":"toolu_1"}`))
	if err != nil {
		t.Fatal(err)
	}

	if got.Tool != "Bash" || string(got.Input) != `{"command":"touch a"}` {
		t.Errorf("request = %+v", got)
	}
}

func TestGivenPermissionToolArgumentsWithoutToolNameWhenParsingThenError(t *testing.T) {
	h := fakeHarness(t)

	if _, err := h.ParsePermission(json.RawMessage(`{"input":{}}`)); err == nil {
		t.Error("no error")
	}
}

func TestGivenAllowWhenEncodingDecisionThenReplyAllowsWithTheOriginalInput(t *testing.T) {
	h := fakeHarness(t)
	req := harness.PermissionRequest{Tool: "Bash", Input: json.RawMessage(`{"command":"touch a"}`)}

	got := h.EncodeDecision(req, harness.Decision{Allow: true})

	want := `{"behavior":"allow","updatedInput":{"command":"touch a"}}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestGivenDenyWhenEncodingDecisionThenReplyDeniesWithTheMessage(t *testing.T) {
	h := fakeHarness(t)
	req := harness.PermissionRequest{Tool: "Bash", Input: json.RawMessage(`{}`)}

	got := h.EncodeDecision(req, harness.Decision{Message: "Not on this machine."})

	want := `{"behavior":"deny","message":"Not on this machine."}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestGivenSessionToResumeWhenStartingThenClaudeResumesIt(t *testing.T) {
	spec := testSpec
	spec.Resume = "d3b7060b-bcd0-4d5e-8775-506cfc91cf06"

	_, init, _ := startFake(t, spec)

	if got, _ := argValue(init.Argv, "--resume"); got != spec.Resume {
		t.Errorf("--resume = %q, want %q; argv %q", got, spec.Resume, init.Argv)
	}
}

func TestGivenNoSessionToResumeWhenStartingThenANewSessionStarts(t *testing.T) {
	_, init, _ := startFake(t, testSpec)

	if slices.Contains(init.Argv, "--resume") {
		t.Errorf("argv = %q", init.Argv)
	}
}

func TestGivenInitAndResultWhenReadingThenBothReportTheSessionAndOtherLinesDoNot(t *testing.T) {
	proc, _, outputs := startFake(t, testSpec)
	if err := proc.Prompt("prompt-1", "hello"); err != nil {
		t.Fatal(err)
	}
	result := read(t, proc)

	if outputs[0].SessionID != "fake-session" {
		t.Errorf("init session = %q", outputs[0].SessionID)
	}
	if result.SessionID != "fake-session" {
		t.Errorf("result session = %q", result.SessionID)
	}
	for _, out := range outputs[1:] {
		if out.SessionID != "" {
			t.Errorf("line %s reports session %q", out.Line, out.SessionID)
		}
	}
}
