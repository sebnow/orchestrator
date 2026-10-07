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
		lt.task.Stop(stopCtx)
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
	exit := lt.task.Stop(ctx)

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
	lt.task.Stop(ctx)

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
	resumePrompt = "Resume the task from where you stopped and finish it."
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

// Cost: one claude process; the three-step turn, cut short by the pause,
// and a resume turn.
func TestLiveGivenThreeStepTaskWhenPausedDuringTheFirstStepThenTheAgentAcknowledgesStopsAndResumes(t *testing.T) {
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
	paused := waitFor(t, ctx, lt.task, "the pause to settle", func(s daemon.State) bool { return !s.Running || s.Pause == daemon.Paused })
	pausedEvents := lt.journal(t)
	pausedAt := pausedEvents[len(pausedEvents)-1].Seq

	if err := lt.task.Prompt(resumePrompt); err != nil {
		t.Fatal(err)
	}
	resumed := waitFor(t, ctx, lt.task, "the resume turn to end", func(s daemon.State) bool { return !s.Running || (s.Pause == daemon.NotPaused && !s.Busy) })
	lt.task.Stop(ctx)

	t.Logf("paused state %+v", paused)
	if !paused.PausePickedUp {
		t.Error("no turn echoed the pause request's uuid")
	}
	if paused.StopNote == "" {
		t.Error("no stop note")
	}
	events := lt.journal(t)
	assertContiguous(t, events)
	acknowledged := 0
	for _, event := range events {
		if event.Kind == protocol.KindPauseAcknowledged && event.Seq <= pausedAt {
			acknowledged++
		}
	}
	if acknowledged == 0 {
		t.Error("the agent did not call acknowledge_pause before the pause settled")
	}
	if interruptedBefore(t, events, pausedAt+1) {
		t.Error("the daemon had to interrupt")
	}
	for _, o := range outputs(t, events) {
		if ids := o.msg.Answering(); ids != nil {
			t.Logf("seq %d %s/%s answers %q", o.event.Seq, o.msg.Type, o.msg.Subtype, ids)
		}
	}
	pings := pingsBefore(t, events, pausedAt+1)
	t.Logf("pings before the pause settled: %d", pings)
	if pings >= 3 {
		t.Errorf("the agent ran all %d pings before stopping", pings)
	}
	res := results(outputs(t, events))
	final, _ := res[len(res)-1].msg.Result()
	t.Logf("results: %d; final %q; resumed state %+v", len(res), final.Result, resumed)
	if !strings.Contains(final.Result, "FINISHED") {
		t.Errorf("final reply = %q, want FINISHED", final.Result)
	}
	if total := pingsBefore(t, events, ^uint64(0)); total < 3 {
		t.Errorf("only %d pings over both turns", total)
	}
}

// Cost: one claude process; the three-step turn, interrupted, possibly a
// turn for the queued pause request, and a one-word follow-up turn.
func TestLiveGivenPauseTheAgentCannotMeetWhenTheLimitPassesThenItIsInterruptedAndTakesAFollowUp(t *testing.T) {
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
	paused := waitFor(t, ctx, lt.task, "the pause to settle", func(s daemon.State) bool { return !s.Running || s.Pause == daemon.Paused })
	if !paused.Running {
		t.Fatalf("harness exited during the pause: %+v", paused.Exit)
	}
	turnsAtPause := paused.TurnsEnded
	pausedEvents := lt.journal(t)
	pausedAt := pausedEvents[len(pausedEvents)-1].Seq
	if err := lt.task.Prompt("Reply with exactly the word AFTER and nothing else."); err != nil {
		t.Fatal(err)
	}
	after := waitFor(t, ctx, lt.task, "the follow-up to end", turnsSettled(turnsAtPause+1))
	lt.task.Stop(ctx)

	t.Logf("paused state %+v; after %+v", paused, after)
	if !after.Running {
		t.Fatalf("harness exited before the follow-up ended: %+v", after.Exit)
	}
	events := lt.journal(t)
	assertContiguous(t, events)
	if !interruptedBefore(t, events, pausedAt+1) {
		t.Error("no control_response before the pause settled: the interrupt did not reach the harness")
	}
	res := results(outputs(t, events))
	for _, o := range res {
		r, _ := o.msg.Result()
		t.Logf("result seq %d %s %s: %q", o.event.Seq, o.msg.Subtype, r.TerminalReason, r.Result)
	}
	final, _ := res[len(res)-1].msg.Result()
	if strings.TrimSpace(final.Result) != "AFTER" {
		t.Errorf("follow-up reply = %q", final.Result)
	}
}
