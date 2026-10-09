//go:build live

// Live tests run the real claude CLI on PATH with the model haiku, under
// whatever login the machine has, and spend subscription quota. Each test
// starts one claude process; the first test to run also runs
// `claude --version` once. Run with:
//
//	go test -tags live -timeout 30m ./internal/daemon/
package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/daemon"
	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

const liveModel = "haiku"

var (
	liveHarnessOnce sync.Once
	liveHarness     *claude.Harness
	liveHarnessErr  error
)

type liveTask struct {
	task     *daemon.Task
	stateDir string
	workdir  string
}

func startLiveTask(t *testing.T, ctx context.Context, prompt string, limits daemon.PauseLimits) liveTask {
	t.Helper()
	liveHarnessOnce.Do(func() {
		liveHarness, liveHarnessErr = claude.New(context.Background(), "claude")
	})
	if liveHarnessErr != nil {
		t.Fatalf("claude harness: %v", liveHarnessErr)
	}
	gateway, err := daemon.StartGateway()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gateway.Close() })
	lt := liveTask{stateDir: t.TempDir(), workdir: t.TempDir()}
	observe := func(event protocol.Event) {
		if event.Kind != protocol.KindHarnessOutput {
			t.Logf("seq %d %s %s", event.Seq, event.Kind, event.Payload)
		}
	}
	d := daemon.New(lt.stateDir, liveHarness, gateway, observe)
	lt.task, err = d.StartTask(ctx, daemon.TaskSpec{
		ID:      protocol.TaskID(strings.ReplaceAll(t.Name(), "/", "_")),
		Prompt:  prompt,
		Workdir: lt.workdir,
		Model:   liveModel,
		Pause:   limits,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		lt.task.Stop(stopCtx, nil)
	})
	return lt
}

func (lt liveTask) journal(t *testing.T) []protocol.Event {
	t.Helper()
	raw, err := os.ReadFile(daemon.JournalPath(lt.stateDir, lt.task.ID()))
	if err != nil {
		t.Fatal(err)
	}
	var events []protocol.Event
	for line := range bytes.Lines(raw) {
		var event protocol.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("journal line %s: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

// output pairs a harness_output event with its parsed stream-json line.
type output struct {
	event protocol.Event
	msg   claude.Message
}

func outputs(t *testing.T, events []protocol.Event) []output {
	t.Helper()
	var out []output
	for _, event := range events {
		if event.Kind != protocol.KindHarnessOutput {
			continue
		}
		msg, err := claude.Parse(event.Payload)
		if err != nil {
			t.Errorf("seq %d does not parse: %v", event.Seq, err)
			continue
		}
		out = append(out, output{event, msg})
	}
	return out
}

func assertContiguous(t *testing.T, events []protocol.Event) {
	t.Helper()
	for idx, event := range events {
		if event.Seq != uint64(idx+1) {
			t.Errorf("journal line %d has seq %d", idx+1, event.Seq)
		}
	}
}

func waitFor(t *testing.T, ctx context.Context, task *daemon.Task, what string, done func(daemon.State) bool) daemon.State {
	t.Helper()
	s, err := task.WaitFor(ctx, done)
	if err != nil {
		t.Fatalf("waiting for %s: %v; state %+v", what, err, s)
	}
	return s
}

func turnsSettled(n int) func(daemon.State) bool {
	return func(s daemon.State) bool { return !s.Running || (s.TurnsEnded >= n && !s.Busy) }
}

func bashCommand(input json.RawMessage) string {
	var in struct {
		Command string `json:"command"`
	}
	json.Unmarshal(input, &in)
	return in.Command
}

// answerPermissions answers every permission request with decide until ctx
// ends.
func answerPermissions(t *testing.T, ctx context.Context, task *daemon.Task, decide func(protocol.PermissionRequested) harness.Decision) {
	go func() {
		for {
			s, err := task.WaitFor(ctx, func(s daemon.State) bool { return len(s.Permissions) > 0 || !s.Running })
			if err != nil || !s.Running {
				return
			}
			for _, req := range s.Permissions {
				decision := decide(req)
				t.Logf("permission %s %s %s: allow=%v", req.RequestID, req.Tool, req.Input, decision.Allow)
				task.AnswerPermission(req.RequestID, decision)
			}
		}
	}()
}

func results(outs []output) []output {
	var res []output
	for _, o := range outs {
		if _, ok := o.msg.Result(); ok {
			res = append(res, o)
		}
	}
	return res
}

// Cost: one claude process, one turn with no tool call.
func TestLiveGivenOneTurnPromptWhenItRunsThenTheJournalHoldsInitAssistantAndResultInSeqOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	lt := startLiveTask(t, ctx, "Reply with exactly the word READY and nothing else.", daemon.PauseLimits{Acknowledge: time.Minute, Cleanup: time.Minute})

	waitFor(t, ctx, lt.task, "the turn to end", turnsSettled(1))
	exit := lt.task.Stop(ctx, nil)

	events := lt.journal(t)
	assertContiguous(t, events)
	if events[0].Kind != protocol.KindHarnessStarted || events[len(events)-1].Kind != protocol.KindHarnessExited {
		t.Errorf("journal runs from %s to %s", events[0].Kind, events[len(events)-1].Kind)
	}
	if events[0].Harness.Name != claude.Name || events[0].Harness.Version == "" {
		t.Errorf("harness = %+v", events[0].Harness)
	}
	t.Logf("exit %+v", exit)
	var order []string
	for _, o := range outputs(t, events) {
		switch {
		case o.msg.Type == claude.TypeSystem && o.msg.Subtype == claude.SubtypeInit:
			order = append(order, "init")
		case o.msg.Type == "assistant":
			order = append(order, "assistant")
		case o.msg.Type == claude.TypeResult:
			order = append(order, "result")
			res, _ := o.msg.Result()
			if o.msg.Subtype != "success" || strings.TrimSpace(res.Result) != "READY" {
				t.Errorf("result %s: %q", o.msg.Subtype, res.Result)
			}
			if o.msg.SessionID == "" || len(o.msg.Answering()) != 1 {
				t.Errorf("result session %q answering %q", o.msg.SessionID, o.msg.Answering())
			}
			t.Logf("result: session %s, num_turns %d, cost %v, usage %+v, user_message_uuid %q",
				o.msg.SessionID, res.NumTurns, res.TotalCostUSD, res.Usage, o.msg.UserMessageUUID)
		}
	}
	first := func(kind string) int {
		for idx, k := range order {
			if k == kind {
				return idx
			}
		}
		return -1
	}
	if first("init") < 0 || first("init") > first("assistant") || first("assistant") > first("result") {
		t.Errorf("harness output order = %q, want init, assistant, result", order)
	}
	for _, event := range events {
		if event.Kind == protocol.KindQuotaObserved {
			t.Logf("quota observed at seq %d: %s", event.Seq, event.Payload)
		}
	}
}

// Cost: one claude process, one turn with two Bash tool calls. The allow
// is held for 70 s to see whether Claude Code abandons a permission
// request that waits longer than a minute.
func TestLiveGivenToolsThatNeedPermissionWhenTheOwnerAllowsOneAndDeniesTheOtherThenTheTurnReportsBoth(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	const hold = 70 * time.Second
	lt := startLiveTask(t, ctx,
		"Use the Bash tool to run `touch spike-allowed.txt`, then use the Bash tool again to run `touch spike-denied.txt`, "+
			"then reply with one line saying which command succeeded and which failed.",
		daemon.PauseLimits{Acknowledge: time.Minute, Cleanup: time.Minute})
	answerPermissions(t, ctx, lt.task, func(req protocol.PermissionRequested) harness.Decision {
		command := bashCommand(req.Input)
		switch {
		case strings.Contains(command, "spike-allowed"):
			time.Sleep(hold)
			return harness.Decision{Allow: true}
		case strings.Contains(command, "spike-denied"):
			return harness.Decision{Message: "Denied by the operator for this test."}
		}
		return harness.Decision{Message: "Not part of this test."}
	})

	waitFor(t, ctx, lt.task, "the turn to end", turnsSettled(1))
	lt.task.Stop(ctx, nil)

	if _, err := os.Stat(filepath.Join(lt.workdir, "spike-allowed.txt")); err != nil {
		t.Errorf("allowed command did not run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(lt.workdir, "spike-denied.txt")); err == nil {
		t.Error("denied command ran")
	}
	events := lt.journal(t)
	assertContiguous(t, events)
	var requested []protocol.Event
	for _, event := range events {
		if event.Kind == protocol.KindPermissionRequested {
			requested = append(requested, event)
		}
	}
	if len(requested) < 2 {
		t.Fatalf("got %d permission requests, want 2", len(requested))
	}
	res := results(outputs(t, events))
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	result, _ := res[0].msg.Result()
	t.Logf("result: %q", result.Result)
	if !strings.Contains(result.Result, "spike-denied") {
		t.Errorf("the reply does not report the denied command: %q", result.Result)
	}
	var denied []string
	for _, denial := range result.PermissionDenials {
		denied = append(denied, bashCommand(denial.ToolInput))
	}
	if len(denied) != 1 || !strings.Contains(denied[0], "spike-denied") {
		t.Errorf("permission_denials = %q", denied)
	}
	waited := res[0].event.Time.Sub(requested[0].Time)
	t.Logf("first permission request at %s, turn ended %s later", requested[0].Time, waited)
	if waited < hold {
		t.Errorf("the turn ended %s after the first request, before the %s hold", waited, hold)
	}
}

const (
	threeStepPrompt = "Use the Bash tool to run `ping -c 5 127.0.0.1` three times, one call after another, " +
		"never in parallel and never in the background. After each call finishes, write the single " +
		"word DONE-1, DONE-2 or DONE-3 before starting the next call. When all three are done, " +
		"reply with exactly FINISHED."
)

// allowPings allows ping and the gateway's own tools, denies anything
// else, and closes firstPing when it allows the first ping.
func allowPings(t *testing.T, firstPing chan<- struct{}) func(protocol.PermissionRequested) harness.Decision {
	var once sync.Once
	return func(req protocol.PermissionRequested) harness.Decision {
		if strings.HasPrefix(req.Tool, "mcp__orchestrator__") {
			t.Logf("gateway tool %s needed permission despite --allowedTools", req.Tool)
			return harness.Decision{Allow: true}
		}
		if req.Tool == "Bash" && strings.HasPrefix(bashCommand(req.Input), "ping") {
			defer once.Do(func() { close(firstPing) })
			return harness.Decision{Allow: true}
		}
		return harness.Decision{Message: "Only ping is allowed in this test."}
	}
}

// pingsBefore counts the ping tool calls in the journal before seq.
func pingsBefore(t *testing.T, events []protocol.Event, seq uint64) int {
	count := 0
	for _, o := range outputs(t, events) {
		if o.event.Seq >= seq || o.msg.Type != "assistant" {
			continue
		}
		var body struct {
			Message struct {
				Content []struct {
					Type  string          `json:"type"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		json.Unmarshal(o.msg.Raw, &body)
		for _, block := range body.Message.Content {
			if block.Type == "tool_use" && strings.HasPrefix(bashCommand(block.Input), "ping") {
				count++
			}
		}
	}
	return count
}

// interruptedBefore reports whether the harness acknowledged an interrupt
// before seq. Stop also interrupts, so later receipts do not count.
func interruptedBefore(t *testing.T, events []protocol.Event, seq uint64) bool {
	for _, o := range outputs(t, events) {
		if o.event.Seq < seq && o.msg.Type == "control_response" {
			return true
		}
	}
	return false
}

// Cost: one claude process; the three-step turn, cut short by the pause.
// The process exits once the pause has settled; cmd/daemon's live test
// resumes such a task in a new process.
func TestLiveGivenThreeStepTaskWhenPausedDuringTheFirstStepThenTheAgentAcknowledgesStopsAndTheProcessExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	lt := startLiveTask(t, ctx, threeStepPrompt, daemon.PauseLimits{Acknowledge: 90 * time.Second, Cleanup: time.Minute})
	firstPing := make(chan struct{})
	answerPermissions(t, ctx, lt.task, allowPings(t, firstPing))

	select {
	case <-firstPing:
	case <-ctx.Done():
		t.Fatal("no ping before the deadline")
	}
	time.Sleep(time.Second)
	if err := lt.task.Pause(); err != nil {
		t.Fatal(err)
	}
	paused := waitFor(t, ctx, lt.task, "the process to exit", func(s daemon.State) bool { return !s.Running })

	t.Logf("paused state %+v", paused)
	if paused.Pause != daemon.Paused || !paused.PausePickedUp || paused.StopNote == "" || paused.SessionID == "" {
		t.Errorf("state after exit = %+v", paused)
	}
	if paused.Exit.ExitCode != 0 || paused.Exit.Error != "" {
		t.Errorf("exit = %+v", paused.Exit)
	}
	events := lt.journal(t)
	assertContiguous(t, events)
	settledAt := uint64(0)
	acknowledged := 0
	for _, event := range events {
		switch event.Kind {
		case protocol.KindPauseAcknowledged:
			acknowledged++
		case protocol.KindPauseSettled:
			settledAt = event.Seq
		}
	}
	if acknowledged == 0 || settledAt == 0 {
		t.Errorf("acknowledgements %d, settled at seq %d", acknowledged, settledAt)
	}
	if interruptedBefore(t, events, settledAt+1) {
		t.Error("the daemon had to interrupt")
	}
	pings := pingsBefore(t, events, settledAt+1)
	t.Logf("pings before the pause settled: %d", pings)
	if pings >= 3 {
		t.Errorf("the agent ran all %d pings before stopping", pings)
	}
}

// Cost: one claude process; the three-step turn, interrupted, and
// possibly a turn for the queued pause request.
func TestLiveGivenPauseTheAgentCannotMeetWhenTheLimitPassesThenItIsInterruptedAndTheProcessExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	lt := startLiveTask(t, ctx, threeStepPrompt, daemon.PauseLimits{Acknowledge: time.Millisecond, Cleanup: time.Millisecond})
	firstPing := make(chan struct{})
	answerPermissions(t, ctx, lt.task, allowPings(t, firstPing))

	select {
	case <-firstPing:
	case <-ctx.Done():
		t.Fatal("no ping before the deadline")
	}
	time.Sleep(time.Second)
	if err := lt.task.Pause(); err != nil {
		t.Fatal(err)
	}
	paused := waitFor(t, ctx, lt.task, "the process to exit", func(s daemon.State) bool { return !s.Running })

	t.Logf("state after exit %+v", paused)
	if paused.Pause != daemon.Paused || paused.Exit.ExitCode != 0 {
		t.Errorf("state after exit = %+v", paused)
	}
	events := lt.journal(t)
	assertContiguous(t, events)
	if !interruptedBefore(t, events, ^uint64(0)) {
		t.Error("no control_response: the interrupt did not reach the harness")
	}
	for _, o := range results(outputs(t, events)) {
		r, _ := o.msg.Result()
		t.Logf("result seq %d %s %s: %q", o.event.Seq, o.msg.Subtype, r.TerminalReason, r.Result)
	}
}

// Cost: one claude process, one turn with one Bash tool call. The allow
// is held for 6 minutes, past the 5-minute idle timeout the MCP page
// (https://code.claude.com/docs/en/mcp.md) gives an HTTP server without a
// per-server timeout, to see whether Claude Code gives up on an
// unanswered --permission-prompt-tool call of its own accord.
func TestLiveGivenPermissionRequestUnansweredForSixMinutesWhenTheOwnerAllowsItThenTheHarnessWasStillWaitingAndTheTurnCompletes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	const hold = 6 * time.Minute
	lt := startLiveTask(t, ctx, "Use the Bash tool to run `touch held.txt`, then reply with exactly DONE.",
		daemon.PauseLimits{Acknowledge: time.Minute, Cleanup: time.Minute})

	s := waitFor(t, ctx, lt.task, "a permission request", func(s daemon.State) bool { return len(s.Permissions) > 0 || !s.Running })
	if !s.Running {
		t.Fatalf("the harness exited before asking for permission: %+v", s.Exit)
	}
	req := s.Permissions[0]
	requestedAt := time.Now()
	t.Logf("permission %s %s %s requested; holding it for %s", req.RequestID, req.Tool, req.Input, hold)
	for time.Since(requestedAt) < hold {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(30 * time.Second):
		}
		s, _ := lt.task.WaitFor(ctx, func(daemon.State) bool { return true })
		t.Logf("after %s: running %v, turns ended %d, pending permissions %d", time.Since(requestedAt).Round(time.Second), s.Running, s.TurnsEnded, len(s.Permissions))
		if !s.Running || s.TurnsEnded > 0 || len(s.Permissions) != 1 || s.Permissions[0].RequestID != req.RequestID {
			t.Fatalf("the harness stopped waiting %s after the request: %+v", time.Since(requestedAt).Round(time.Second), s)
		}
	}
	if err := lt.task.AnswerPermission(req.RequestID, harness.Decision{Allow: true}); err != nil {
		t.Fatalf("answer after %s: %v", time.Since(requestedAt).Round(time.Second), err)
	}
	answeredAt := time.Now()
	answerPermissions(t, ctx, lt.task, func(protocol.PermissionRequested) harness.Decision {
		return harness.Decision{Message: "Not part of this test."}
	})
	waitFor(t, ctx, lt.task, "the turn to end", turnsSettled(1))
	lt.task.Stop(ctx, nil)

	if _, err := os.Stat(filepath.Join(lt.workdir, "held.txt")); err != nil {
		t.Errorf("the allowed command did not run: %v", err)
	}
	events := lt.journal(t)
	assertContiguous(t, events)
	var requestSeq uint64
	for _, event := range events {
		if event.Kind == protocol.KindPermissionRequested && requestSeq == 0 {
			requestSeq = event.Seq
		}
	}
	var during []string
	for _, o := range outputs(t, events) {
		if o.event.Seq > requestSeq && o.event.Time.Before(answeredAt) {
			during = append(during, o.msg.Type+"/"+o.msg.Subtype)
		}
	}
	t.Logf("harness output between the request (seq %d) and the answer: %q", requestSeq, during)
	res := results(outputs(t, events))
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	result, _ := res[0].msg.Result()
	t.Logf("result %s after %s: %q", res[0].msg.Subtype, res[0].event.Time.Sub(requestedAt).Round(time.Second), result.Result)
	if res[0].msg.Subtype != "success" || !strings.Contains(result.Result, "DONE") {
		t.Errorf("result %s: %q", res[0].msg.Subtype, result.Result)
	}
}

// Cost: one claude process, one turn in which the agent starts one of
// Claude Code's own subagents with its Agent tool. Every stream-json line
// is logged, prefixed "raw:", so that the subagent's messages can be
// compared with internal/harness/claude/testdata/subagent.jsonl.
func TestLiveGivenPromptToUseASubagentWhenItRunsThenTheSubagentsMessagesCarryTheAgentCallAsTheirParent(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	lt := startLiveTask(t, ctx, "Use a subagent to count the files in this directory and then reply with the number only.",
		daemon.PauseLimits{Acknowledge: time.Minute, Cleanup: time.Minute})
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(lt.workdir, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	answerPermissions(t, ctx, lt.task, func(protocol.PermissionRequested) harness.Decision {
		return harness.Decision{Allow: true}
	})

	waitFor(t, ctx, lt.task, "the turn to end", turnsSettled(1))
	lt.task.Stop(ctx, nil)

	events := lt.journal(t)
	var agentCall string
	children := map[string]int{}
	var childText bool
	for _, o := range outputs(t, events) {
		t.Logf("raw: %s", o.event.Payload)
		var line struct {
			ParentToolUseID *string `json:"parent_tool_use_id"`
			Message         struct {
				Content []struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
		}
		json.Unmarshal(o.event.Payload, &line)
		if line.ParentToolUseID == nil {
			for _, block := range line.Message.Content {
				if block.Type == "tool_use" && (block.Name == "Agent" || block.Name == "Task") && agentCall == "" {
					agentCall = block.ID
				}
			}
			continue
		}
		children[o.msg.Type+"/"+o.msg.Subtype]++
		for _, block := range line.Message.Content {
			if o.msg.Type == "assistant" && block.Type == "text" {
				childText = true
			}
		}
		for _, body := range claude.Normalise(o.event.Payload) {
			if transcript.ParentToolUseID(body) != *line.ParentToolUseID {
				t.Errorf("seq %d: body %T has parent %q, want %q", o.event.Seq, body, transcript.ParentToolUseID(body), *line.ParentToolUseID)
			}
		}
	}
	t.Logf("agent call %q; lines with a parent, by type: %v; subagent assistant text: %v", agentCall, children, childText)
	if agentCall == "" {
		t.Fatal("the agent started no subagent")
	}
	if len(children) == 0 {
		t.Error("no line carries a parent_tool_use_id")
	}
}
