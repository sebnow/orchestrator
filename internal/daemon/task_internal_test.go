package daemon

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

type taskFixture struct {
	task     *Task
	proc     *fakeProcess
	stateDir string
	events   *eventLog
}

// eventLog collects what the daemon shows its observer.
type eventLog struct {
	mu     sync.Mutex
	events []protocol.Event
}

func (l *eventLog) observe(event protocol.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) all() []protocol.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

var testTaskSpec = TaskSpec{
	ID: "task-1", Prompt: "Do the work.", Workdir: "/work", Model: "fake-model", SystemPrompt: "Be brief.",
	Pause: PauseLimits{Acknowledge: 2 * time.Minute, Cleanup: 5 * time.Minute},
}

func startTestTask(t *testing.T, gateway *Gateway, spec TaskSpec) taskFixture {
	t.Helper()
	h := newFakeHarness()
	f := taskFixture{stateDir: t.TempDir(), events: &eventLog{}}
	d := New(f.stateDir, h, gateway, f.events.observe)
	task, err := d.StartTask(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	f.task = task
	f.proc = <-h.started
	return f
}

func (f taskFixture) journal(t *testing.T) []protocol.Event {
	t.Helper()
	return readJournalFile(t, JournalPath(f.stateDir, f.task.ID()))
}

// end makes the fake harness exit and waits until the task has recorded it.
func (f taskFixture) end(t *testing.T, exit protocol.HarnessExited) {
	t.Helper()
	f.proc.end(exit)
	<-f.task.Done()
}

func kinds(events []protocol.Event) []protocol.Kind {
	var out []protocol.Kind
	for _, event := range events {
		out = append(out, event.Kind)
	}
	return out
}

func waitFor(t *testing.T, task *Task, what string, done func(State) bool) State {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	s, err := task.WaitFor(ctx, done)
	if err != nil {
		t.Fatalf("waiting for %s: %v; state %+v", what, err, s)
	}
	return s
}

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestGivenTaskSpecWhenStartingThenTheHarnessGetsTheSpecAndThePromptWithAUUID(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	defer f.end(t, protocol.HarnessExited{})

	spec := f.proc.spec
	if spec.Workdir != "/work" || spec.Model != "fake-model" || spec.SystemPrompt != "Be brief." {
		t.Errorf("spec = %+v", spec)
	}
	if !strings.HasSuffix(spec.Gateway.URL, "/tasks/task-1/mcp") || spec.Gateway.PermissionTool != PermissionTool ||
		!slices.Equal(spec.Gateway.Tools, []string{AcknowledgePauseTool, SpawnTaskTool, SendMessageTool}) {
		t.Errorf("gateway = %+v", spec.Gateway)
	}
	prompt := f.proc.nextInput(t)
	if prompt.kind != "prompt" || prompt.text != "Do the work." || !uuidPattern.MatchString(prompt.id) {
		t.Errorf("first input = %+v", prompt)
	}
	if !f.task.State().Busy {
		t.Error("task not busy with its prompt")
	}
}

func TestGivenStartedTaskWhenReadingTheJournalThenHarnessStartedIsSeqOne(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	f.end(t, protocol.HarnessExited{})

	events := f.journal(t)

	if events[0].Seq != 1 || events[0].Kind != protocol.KindHarnessStarted {
		t.Fatalf("first event = %+v", events[0])
	}
	if got := string(events[0].Payload); got != `{"pid":4242,"model":"fake-model","workdir":"/work"}` {
		t.Errorf("payload = %s", got)
	}
}

func TestGivenHarnessOutputWhenTheTurnEndsThenEachLineIsJournaledInOrderAndTheTaskIsIdle(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	prompt := f.proc.nextInput(t)

	f.proc.emit(harness.Output{Line: []byte(`{"n":1}`)})
	f.proc.emit(harness.Output{Line: []byte(`{"n":2}`), TurnEnded: true, Answering: []string{prompt.id}})
	s := waitFor(t, f.task, "turn end", func(s State) bool { return s.TurnsEnded == 1 })
	f.end(t, protocol.HarnessExited{ExitCode: 0})

	if s.Busy {
		t.Error("task still busy after the turn answering its prompt ended")
	}
	events := f.journal(t)
	want := []protocol.Kind{protocol.KindHarnessStarted, protocol.KindHarnessOutput, protocol.KindHarnessOutput, protocol.KindHarnessExited}
	if !slices.Equal(kinds(events), want) {
		t.Fatalf("kinds = %v, want %v", kinds(events), want)
	}
	for idx, event := range events {
		if event.Seq != uint64(idx+1) {
			t.Errorf("event %d has seq %d", idx, event.Seq)
		}
	}
	if string(events[1].Payload) != `{"n":1}` || string(events[2].Payload) != `{"n":2}` {
		t.Errorf("payloads = %s, %s", events[1].Payload, events[2].Payload)
	}
	if !slices.EqualFunc(f.events.all(), events, func(a, b protocol.Event) bool { return a.Seq == b.Seq && a.Kind == b.Kind }) {
		t.Errorf("observer saw %v, journal has %v", kinds(f.events.all()), kinds(events))
	}
}

func TestGivenTurnEndAnsweringNoPromptWhenItEndsThenThePromptStaysOutstanding(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	defer f.end(t, protocol.HarnessExited{})

	f.proc.emit(harness.Output{Line: []byte(`{}`), TurnEnded: true})
	s := waitFor(t, f.task, "turn end", func(s State) bool { return s.TurnsEnded == 1 })

	if !s.Busy {
		t.Error("task idle although no turn answered its prompt")
	}
}

func TestGivenQuotaInHarnessOutputWhenJournalingThenQuotaObservedFollowsTheLine(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	quota := protocol.QuotaObserved{Status: protocol.QuotaAllowed, Windows: []protocol.QuotaWindow{{Name: "five_hour", Utilization: 0.3}}}

	f.proc.emit(harness.Output{Line: []byte(`{"type":"rate"}`), Quota: &quota})
	f.end(t, protocol.HarnessExited{})

	events := f.journal(t)
	want := []protocol.Kind{protocol.KindHarnessStarted, protocol.KindHarnessOutput, protocol.KindQuotaObserved, protocol.KindHarnessExited}
	if !slices.Equal(kinds(events), want) {
		t.Fatalf("kinds = %v, want %v", kinds(events), want)
	}
	if !strings.Contains(string(events[2].Payload), `"name":"five_hour","utilization":0.3`) {
		t.Errorf("payload = %s", events[2].Payload)
	}
}

func callPermission(t *testing.T, url string, arguments map[string]any) <-chan string {
	t.Helper()
	session := mustConnect(t, url)
	reply := make(chan string, 1)
	go func() {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: PermissionTool, Arguments: arguments})
		if err != nil {
			reply <- "error: " + err.Error()
			return
		}
		reply <- result.Content[0].(*mcp.TextContent).Text
	}()
	return reply
}

func TestGivenPermissionRequestWhenTheOwnerAllowsThenItIsJournaledAndTheHarnessIsAllowed(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	defer f.end(t, protocol.HarnessExited{})

	reply := callPermission(t, f.proc.spec.Gateway.URL, map[string]any{"tool": "Bash", "input": map[string]any{"command": "touch a"}})
	s := waitFor(t, f.task, "permission request", func(s State) bool { return len(s.Permissions) == 1 })

	events := f.journal(t)
	last := events[len(events)-1]
	if last.Kind != protocol.KindPermissionRequested || !strings.Contains(string(last.Payload), `"tool":"Bash","input":{"command":"touch a"}`) {
		t.Fatalf("last journal event = %s %s", last.Kind, last.Payload)
	}
	request := s.Permissions[0]
	if !strings.Contains(string(last.Payload), request.RequestID) {
		t.Errorf("journaled %s, pending %+v", last.Payload, request)
	}
	if err := f.task.AnswerPermission(request.RequestID, harness.Decision{Allow: true}); err != nil {
		t.Fatal(err)
	}
	if got := <-reply; got != "allow Bash" {
		t.Errorf("reply = %q", got)
	}
	if s := f.task.State(); len(s.Permissions) != 0 {
		t.Errorf("still pending: %+v", s.Permissions)
	}
}

func TestGivenPermissionRequestWhenTheOwnerDeniesThenTheHarnessGetsTheMessage(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	defer f.end(t, protocol.HarnessExited{})

	reply := callPermission(t, f.proc.spec.Gateway.URL, map[string]any{"tool": "Bash", "input": map[string]any{}})
	s := waitFor(t, f.task, "permission request", func(s State) bool { return len(s.Permissions) == 1 })
	f.task.AnswerPermission(s.Permissions[0].RequestID, harness.Decision{Message: "Not here."})

	if got := <-reply; got != "deny: Not here." {
		t.Errorf("reply = %q", got)
	}
}

func TestGivenAnsweredPermissionWhenAnsweringAgainThenErrStale(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	defer f.end(t, protocol.HarnessExited{})
	reply := callPermission(t, f.proc.spec.Gateway.URL, map[string]any{"tool": "Bash"})
	s := waitFor(t, f.task, "permission request", func(s State) bool { return len(s.Permissions) == 1 })
	f.task.AnswerPermission(s.Permissions[0].RequestID, harness.Decision{Allow: true})
	<-reply

	err := f.task.AnswerPermission(s.Permissions[0].RequestID, harness.Decision{Allow: true})

	if !errors.Is(err, ErrStale) {
		t.Errorf("err = %v, want ErrStale", err)
	}
}

func TestGivenPendingPermissionWhenTheHarnessExitsThenTheRequestIsDeniedAndExitIsJournaledLast(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	reply := callPermission(t, f.proc.spec.Gateway.URL, map[string]any{"tool": "Bash"})
	waitFor(t, f.task, "permission request", func(s State) bool { return len(s.Permissions) == 1 })

	f.end(t, protocol.HarnessExited{ExitCode: 1, Stderr: "boom"})

	if got := <-reply; !strings.HasPrefix(got, "deny: ") && !strings.HasPrefix(got, "error: ") {
		t.Errorf("reply = %q, want a denial or a closed connection", got)
	}
	events := f.journal(t)
	last := events[len(events)-1]
	if last.Kind != protocol.KindHarnessExited || string(last.Payload) != `{"exit_code":1,"stderr":"boom"}` {
		t.Errorf("last event = %s %s", last.Kind, last.Payload)
	}
	if s := f.task.State(); s.Running || s.Exit == nil || s.Exit.ExitCode != 1 {
		t.Errorf("state = %+v", s)
	}
}

func TestGivenRunningTaskWhenStoppingThenItInterruptsClosesInputAndReturnsTheExit(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	f.proc.nextInput(t)

	exit := f.task.Stop(t.Context(), nil)

	if got := []string{f.proc.nextInput(t).kind, f.proc.nextInput(t).kind}; !slices.Equal(got, []string{"interrupt", "close"}) {
		t.Errorf("inputs = %q", got)
	}
	if exit.ExitCode != 0 {
		t.Errorf("exit = %+v", exit)
	}
	if err := f.task.Prompt("more"); !errors.Is(err, ErrTaskEnded) {
		t.Errorf("prompt after stop = %v, want ErrTaskEnded", err)
	}
	if err := f.task.Interrupt(); !errors.Is(err, ErrTaskEnded) {
		t.Errorf("interrupt after stop = %v, want ErrTaskEnded", err)
	}
}

// stopExitingWith stops f's task with cutShort and has the harness exit
// with own once its input is closed, and returns what Stop returned.
func stopExitingWith(t *testing.T, f taskFixture, own protocol.HarnessExited, cutShort func(string, protocol.HarnessExited) string) protocol.HarnessExited {
	t.Helper()
	f.proc.endOnClose = false
	stopped := make(chan protocol.HarnessExited, 1)
	go func() { stopped <- f.task.Stop(t.Context(), cutShort) }()
	for f.proc.nextInput(t).kind != "close" {
	}
	f.proc.end(own)
	return <-stopped
}

func TestGivenRunningTurnWhenStopCutsItShortAndTheHarnessExitsWithAnErrorThenTheExitSaysSoAndKeepsTheStderr(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	f.proc.nextInput(t)
	f.proc.emit(harness.Output{Line: []byte(`{"type":"system"}`), SessionID: "session-1"})
	var gotSession string
	var gotOwn protocol.HarnessExited

	exit := stopExitingWith(t, f, protocol.HarnessExited{ExitCode: 1, Stderr: "tail"}, func(session string, own protocol.HarnessExited) string {
		gotSession, gotOwn = session, own
		return "cut short"
	})

	if want := (protocol.HarnessExited{ExitCode: -1, Error: "cut short", Stderr: "tail"}); exit != want {
		t.Errorf("exit = %+v, want %+v", exit, want)
	}
	if gotSession != "session-1" || gotOwn.ExitCode != 1 {
		t.Errorf("cutShort got session %q, own exit %+v", gotSession, gotOwn)
	}
	if s := f.task.State(); !s.CutShort {
		t.Errorf("state = %+v, want cut short", s)
	}
	events := f.journal(t)
	if last := events[len(events)-1]; string(last.Payload) != `{"exit_code":-1,"error":"cut short","stderr":"tail"}` {
		t.Errorf("last event = %s %s", last.Kind, last.Payload)
	}
}

func TestGivenRunningTurnWhenStopCutsItShortThenItsOwnExitIsKeptIfCleanOrIfCutShortGivesNoError(t *testing.T) {
	for _, tc := range []struct {
		name string
		own  protocol.HarnessExited
		text string
	}{
		{"clean exit", protocol.HarnessExited{}, "cut short"},
		{"no error given", protocol.HarnessExited{ExitCode: 1}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startTestTask(t, startTestGateway(t), testTaskSpec)
			f.proc.nextInput(t)

			exit := stopExitingWith(t, f, tc.own, func(string, protocol.HarnessExited) string { return tc.text })

			if exit != tc.own || f.task.State().CutShort {
				t.Errorf("exit = %+v, state %+v; want the harness's own exit", exit, f.task.State())
			}
		})
	}
}

func TestGivenTurnThatHadEndedWhenStoppingThenNothingIsCutShort(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	f.proc.endOnClose = false
	in := f.proc.nextInput(t)
	f.proc.emit(harness.Output{Line: []byte(`{"type":"result"}`), TurnEnded: true, Answering: []string{in.id}})
	if in := f.proc.nextInput(t); in.kind != "close" {
		t.Fatalf("input = %+v, want the input closed", in)
	}
	stopped := make(chan protocol.HarnessExited, 1)
	go func() {
		stopped <- f.task.Stop(t.Context(), func(string, protocol.HarnessExited) string { return "cut short" })
	}()
	f.proc.end(protocol.HarnessExited{ExitCode: 1})

	if exit := <-stopped; exit.ExitCode != 1 || exit.Error != "" || f.task.State().CutShort {
		t.Errorf("exit = %+v, want the harness's own", exit)
	}
	f.proc.noInput(t)
}

func TestGivenHarnessIgnoringClosedInputWhenStopTimesOutThenItIsKilled(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	f.proc.endOnClose = false
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	exit := f.task.Stop(ctx, nil)

	if exit.ExitCode != -1 || exit.Error != "signal: killed" {
		t.Errorf("exit = %+v", exit)
	}
}

func TestGivenRunningTaskWhenInterruptingThenTheHarnessGetsAnInterrupt(t *testing.T) {
	f := startTestTask(t, startTestGateway(t), testTaskSpec)
	defer f.end(t, protocol.HarnessExited{})
	f.proc.nextInput(t)

	if err := f.task.Interrupt(); err != nil {
		t.Fatal(err)
	}

	if in := f.proc.nextInput(t); in.kind != "interrupt" {
		t.Errorf("input = %+v", in)
	}
}

func TestGivenTaskWithAJournalWhenStartingItAgainThenErrTaskExistsAndNoHarnessStarts(t *testing.T) {
	gateway := startTestGateway(t)
	h := newFakeHarness()
	stateDir := t.TempDir()
	d := New(stateDir, h, gateway, nil)
	first, err := d.StartTask(t.Context(), testTaskSpec)
	if err != nil {
		t.Fatal(err)
	}
	(<-h.started).end(protocol.HarnessExited{})
	<-first.Done()

	_, err = d.StartTask(t.Context(), testTaskSpec)

	if !errors.Is(err, ErrTaskExists) {
		t.Errorf("err = %v, want ErrTaskExists", err)
	}
	if len(h.started) != 0 {
		t.Error("a second harness was started")
	}
}

func TestGivenHarnessThatCannotStartWhenStartingTaskThenTheJournalRecordsWhy(t *testing.T) {
	h := newFakeHarness()
	h.startErr = errors.New("no claude on PATH")
	stateDir := t.TempDir()
	d := New(stateDir, h, startTestGateway(t), nil)

	_, err := d.StartTask(t.Context(), testTaskSpec)

	if err == nil {
		t.Fatal("no error")
	}
	events := readJournalFile(t, JournalPath(stateDir, "task-1"))
	if len(events) != 1 || events[0].Kind != protocol.KindHarnessExited ||
		string(events[0].Payload) != `{"exit_code":-1,"error":"no claude on PATH"}` {
		t.Errorf("journal = %+v", events)
	}
}
