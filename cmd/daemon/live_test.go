//go:build live

// The live tests build cmd/server and cmd/daemon, run them, and have the
// daemon run the real claude CLI on PATH with the model haiku, under
// whatever login the machine has. They spend subscription quota: each
// test's daemon runs `claude --version` once, and each test's comment
// names the claude sessions it runs. Run with:
//
//	go test -tags live -run Live -v ./cmd/daemon/
package main_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// syncBuffer collects a process's output while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func buildBinaries(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	build := exec.Command("go", "build", "-o", dir,
		"github.com/sebnow/orchestrator/cmd/server", "github.com/sebnow/orchestrator/cmd/daemon")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return dir
}

// startProcess starts a binary, keeps its stderr, and on cleanup sends it
// SIGTERM and checks that it exits with 0.
func startProcess(t *testing.T, name string, stderr io.Writer, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		exited := make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		select {
		case err := <-exited:
			if err != nil {
				t.Errorf("%s after SIGTERM: %v", filepath.Base(name), err)
			}
		case <-time.After(90 * time.Second):
			cmd.Process.Kill()
			t.Errorf("%s did not exit within 90 s of SIGTERM", filepath.Base(name))
		}
	})
	return cmd
}

// daemonCredentials issues the daemon live-daemon a certificate from a
// new CA and returns the flags that give it to cmd/daemon. The server runs
// with -insecure-loopback, so the certificate only names the daemon.
func daemonCredentials(t *testing.T) []string {
	t.Helper()
	ca, err := pki.NewCA()
	if err != nil {
		t.Fatal(err)
	}
	issued, err := ca.IssueDaemon("live-daemon")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := issued.Write(dir, "daemon"); err != nil {
		t.Fatal(err)
	}
	return []string{"-cert", filepath.Join(dir, "daemon.crt"), "-key", filepath.Join(dir, "daemon.key")}
}

var serving = regexp.MustCompile(`msg=serving address=(\S+)`)

// startServer runs cmd/server on a free loopback port, with extra flags,
// and returns its base URL, read from its log.
func startServer(t *testing.T, bin string, extra ...string) string {
	t.Helper()
	reader, writer := io.Pipe()
	logs := &syncBuffer{}
	startProcess(t, filepath.Join(bin, "server"), writer,
		append([]string{"-insecure-loopback", "-listen", "127.0.0.1:0", "-db", filepath.Join(t.TempDir(), "server.db")}, extra...)...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("server log:\n%s", logs)
		}
	})
	address := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			fmt.Fprintln(logs, scanner.Text())
			if m := serving.FindStringSubmatch(scanner.Text()); m != nil {
				address <- m[1]
			}
		}
	}()
	select {
	case addr := <-address:
		return "http://" + addr
	case <-time.After(30 * time.Second):
		t.Fatalf("server did not report its address; log:\n%s", logs)
		return ""
	}
}

func call(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode/100 == 2 && out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: decode %s: %v", method, url, data, err)
		}
	}
	if resp.StatusCode/100 != 2 {
		t.Logf("%s %s: %d %s", method, url, resp.StatusCode, bytes.TrimSpace(data))
	}
	return resp.StatusCode
}

// waitForEvents polls the task's transcript until done accepts it.
func waitForEvents(t *testing.T, ctx context.Context, url, what string, done func([]protocol.Event) bool) []protocol.Event {
	t.Helper()
	for {
		var events []protocol.Event
		call(t, http.MethodGet, url, nil, &events)
		if done(events) {
			return events
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v; %d events", what, ctx.Err(), len(events))
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func hasKind(kind protocol.Kind) func([]protocol.Event) bool {
	return func(events []protocol.Event) bool {
		for _, event := range events {
			if event.Kind == kind {
				return true
			}
		}
		return false
	}
}

func hasResult(events []protocol.Event) bool {
	for _, event := range events {
		if event.Kind != protocol.KindHarnessOutput {
			continue
		}
		if msg, err := claude.Parse(event.Payload); err == nil && msg.Type == claude.TypeResult {
			return true
		}
	}
	return false
}

// Cost: `claude --version` and one claude session with one turn and no
// tool call.
func TestLiveGivenServerAndDaemonBinariesWhenTheOwnerRunsAOneTurnTaskThenItsTranscriptIsReadableFromTheServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	bin := buildBinaries(t)
	server := startServer(t, bin)
	daemonLogs := &syncBuffer{}
	startProcess(t, filepath.Join(bin, "daemon"), daemonLogs,
		append([]string{"-server", server, "-state-dir", t.TempDir()}, daemonCredentials(t)...)...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", daemonLogs)
		}
	})

	var start protocol.Command
	for {
		status := call(t, http.MethodPost, server+"/v1/tasks", map[string]any{
			"daemon_id":    "live-daemon",
			"prompt":       "Reply with exactly the word READY and nothing else.",
			"model":        "haiku",
			"pause_limits": map[string]string{"acknowledge": "1m0s", "cleanup": "1m0s"},
		}, &start)
		if status == http.StatusCreated {
			break
		}
		if status != http.StatusUnprocessableEntity || ctx.Err() != nil {
			t.Fatalf("create task: %d", status)
		}
		time.Sleep(200 * time.Millisecond)
	}
	transcript := server + "/v1/tasks/" + string(start.TaskID) + "/events"
	waitForEvents(t, ctx, transcript, "the turn's result", hasResult)
	call(t, http.MethodPost, server+"/v1/tasks/"+string(start.TaskID)+"/commands", map[string]string{"kind": "stop"}, nil)
	events := waitForEvents(t, ctx, transcript, "harness_exited", hasKind(protocol.KindHarnessExited))

	for idx, event := range events {
		if event.Seq != uint64(idx+1) {
			t.Fatalf("event %d has seq %d", idx, event.Seq)
		}
	}
	if events[0].Kind != protocol.KindHarnessStarted || events[len(events)-1].Kind != protocol.KindHarnessExited {
		t.Errorf("transcript runs from %s to %s", events[0].Kind, events[len(events)-1].Kind)
	}
	if events[0].Harness.Name != claude.Name || events[0].Harness.Version == "" {
		t.Errorf("harness = %+v", events[0].Harness)
	}
	var order []string
	for _, event := range events {
		if event.Kind != protocol.KindHarnessOutput {
			t.Logf("seq %d %s %s", event.Seq, event.Kind, event.Payload)
			continue
		}
		msg, err := claude.Parse(event.Payload)
		if err != nil {
			t.Errorf("seq %d does not parse: %v", event.Seq, err)
			continue
		}
		switch {
		case msg.Type == claude.TypeSystem && msg.Subtype == claude.SubtypeInit:
			order = append(order, "init")
		case msg.Type == "assistant":
			order = append(order, "assistant")
		case msg.Type == claude.TypeResult:
			order = append(order, "result")
			res, _ := msg.Result()
			if msg.Subtype != "success" || strings.TrimSpace(res.Result) != "READY" {
				t.Errorf("result %s: %q", msg.Subtype, res.Result)
			}
		}
	}
	init, assistant, result := slices.Index(order, "init"), slices.Index(order, "assistant"), slices.Index(order, "result")
	if init < 0 || init > assistant || assistant > result || slices.Index(order[result+1:], "result") >= 0 {
		t.Errorf("harness output = %q, want init, assistant, then one result", order)
	}
}

// liveSystem is cmd/server and cmd/daemon running together, the daemon's
// claude wrapped by a script that logs each invocation.
type liveSystem struct {
	server      string
	invocations string
	// daemon and daemonArgs run cmd/daemon against the server with the
	// wrapped claude and the system's one state directory.
	daemon     string
	daemonArgs []string
	// answered holds the permission requests the test has answered.
	answered map[string]bool
	// answeredByPolicy says the server answers permission requests itself,
	// so the test leaves them alone.
	answeredByPolicy bool
}

// startLiveSystem starts the server, with serverFlags, and the daemon.
func startLiveSystem(t *testing.T, serverFlags ...string) liveSystem {
	t.Helper()
	sys := newLiveSystem(t, serverFlags...)
	daemonLogs := &syncBuffer{}
	startProcess(t, sys.daemon, daemonLogs, sys.daemonArgs...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", daemonLogs)
		}
	})
	return sys
}

// newLiveSystem starts the server, with serverFlags, and prepares the
// daemon's command line without starting the daemon. The server leaves
// permission requests to the test, as the owner, unless serverFlags say
// otherwise.
func newLiveSystem(t *testing.T, serverFlags ...string) liveSystem {
	t.Helper()
	bin := buildBinaries(t)
	sys := liveSystem{server: startServer(t, bin, append([]string{"-permissions", "ask"}, serverFlags...)...), invocations: filepath.Join(t.TempDir(), "invocations.log"), answered: map[string]bool{}}
	wrapper := filepath.Join(bin, "claude-counting")
	// Only the first argument is logged: the others include the system
	// prompt, whose line breaks would count as further invocations.
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> '" + sys.invocations + "'\nexec claude \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	sys.daemon = filepath.Join(bin, "daemon")
	sys.daemonArgs = append([]string{"-server", sys.server, "-state-dir", t.TempDir(), "-claude", wrapper}, daemonCredentials(t)...)
	t.Cleanup(func() {
		data, _ := os.ReadFile(sys.invocations)
		sessions := 0
		for line := range strings.Lines(string(data)) {
			if !strings.HasPrefix(line, "--version") {
				sessions++
			}
		}
		t.Logf("claude model sessions: %d", sessions)
	})
	return sys
}

var problemText = regexp.MustCompile(`<p class="problem" role="alert">([^<]*)</p>`)

// postForm posts a GUI form as a browser without JavaScript would, and
// returns the status and the redirect target.
func postForm(t *testing.T, target string, form url.Values) (int, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusSeeOther {
		if problem := problemText.FindSubmatch(body); problem != nil {
			body = problem[1]
		}
		t.Logf("POST %s: %d %s", target, resp.StatusCode, bytes.TrimSpace(body))
	}
	return resp.StatusCode, resp.Header.Get("Location")
}

// startTaskViaGUI starts a task through the new-task form once the daemon
// has connected, with the form's other fields set from extra.
func (sys liveSystem) startTaskViaGUI(t *testing.T, ctx context.Context, prompt string, extra ...string) protocol.TaskID {
	t.Helper()
	form := url.Values{"prompt": {prompt}, "repo": {""}, "ref": {""}, "model": {"haiku"}, "daemon": {"live-daemon"},
		"acknowledge": {"90s"}, "cleanup": {"1m"}}
	for idx := 0; idx+1 < len(extra); idx += 2 {
		form.Set(extra[idx], extra[idx+1])
	}
	for {
		status, location := postForm(t, sys.server+"/tasks", form)
		if task, ok := strings.CutPrefix(location, "/tasks/"); status == http.StatusSeeOther && ok {
			return protocol.TaskID(task)
		}
		if ctx.Err() != nil {
			t.Fatalf("start task: %d", status)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (sys liveSystem) command(t *testing.T, task protocol.TaskID, form url.Values) {
	t.Helper()
	if status, _ := postForm(t, sys.server+"/tasks/"+string(task)+"/commands", form); status != http.StatusSeeOther {
		t.Fatalf("command %v: status %d", form, status)
	}
}

func (sys liveSystem) state(t *testing.T, task protocol.TaskID) string {
	t.Helper()
	var detail struct {
		State string `json:"state"`
	}
	call(t, http.MethodGet, sys.server+"/v1/tasks/"+string(task), nil, &detail)
	return detail.State
}

func (sys liveSystem) events(t *testing.T, task protocol.TaskID) []protocol.Event {
	t.Helper()
	var events []protocol.Event
	call(t, http.MethodGet, sys.server+"/v1/tasks/"+string(task)+"/events", nil, &events)
	return events
}

// waitForState polls until the task is in state with every event of
// exits processes stored, answering each permission request with decide
// through the task page's form on the way. decide may issue commands.
func (sys liveSystem) waitForState(t *testing.T, ctx context.Context, task protocol.TaskID, state string, exits int, decide func(tool, command string) bool) []protocol.Event {
	t.Helper()
	return sys.waitForStateEach(t, ctx, task, state, exits, decide, nil)
}

// waitForStateEach is waitForState calling each, when set, after every
// poll.
func (sys liveSystem) waitForStateEach(t *testing.T, ctx context.Context, task protocol.TaskID, state string, exits int, decide func(tool, command string) bool, each func()) []protocol.Event {
	t.Helper()
	for {
		events := sys.events(t, task)
		sys.answerPermissions(t, task, events, decide)
		if each != nil {
			each()
		}
		got := sys.state(t, task)
		if got == state && countKind(events, protocol.KindHarnessExited) >= exits {
			return events
		}
		if got == "failed" || got == "stopped" {
			t.Fatalf("task is %s waiting for %s", got, state)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v; state %s, %d events", state, ctx.Err(), got, len(events))
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func countKind(events []protocol.Event, kind protocol.Kind) int {
	count := 0
	for _, event := range events {
		if event.Kind == kind {
			count++
		}
	}
	return count
}

// sessionsAndResults returns the session id of every init and result,
// and the text of every result, in order.
func sessionsAndResults(t *testing.T, events []protocol.Event) (sessions []string, results []string) {
	t.Helper()
	for idx, event := range events {
		if event.Seq != uint64(idx+1) {
			t.Fatalf("event %d has seq %d", idx, event.Seq)
		}
		if event.Kind != protocol.KindHarnessOutput {
			t.Logf("seq %d %s %s", event.Seq, event.Kind, event.Payload)
			continue
		}
		msg, err := claude.Parse(event.Payload)
		if err != nil {
			continue
		}
		if _, ok := msg.Init(); ok {
			sessions = append(sessions, msg.SessionID)
		}
		if res, ok := msg.Result(); ok {
			sessions = append(sessions, msg.SessionID)
			results = append(results, res.Result)
			t.Logf("seq %d result %s session %s cost %v: %q", event.Seq, msg.Subtype, msg.SessionID, res.TotalCostUSD, res.Result)
		}
	}
	return sessions, results
}

func requireOneSession(t *testing.T, sessions []string) {
	t.Helper()
	if len(sessions) == 0 {
		t.Fatal("no session id")
	}
	for _, session := range sessions {
		if session != sessions[0] {
			t.Errorf("session ids = %q, want one", sessions)
		}
	}
}

// Cost: `claude --version` and two claude sessions, each one turn with no
// tool call.
func TestLiveGivenFinishedTaskWhenTheOwnerFollowsUpThenANewProcessResumesTheConversation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Minute)
	defer cancel()
	sys := startLiveSystem(t)

	task := sys.startTaskViaGUI(t, ctx, "The codeword is MARMALADE. Remember it. Reply with exactly OK.")
	sys.waitForState(t, ctx, task, "finished", 1, nil)
	sys.command(t, task, url.Values{"kind": {"prompt"}, "text": {"What is the codeword? Reply with the codeword only."}})
	events := sys.waitForState(t, ctx, task, "finished", 2, nil)

	sessions, results := sessionsAndResults(t, events)
	requireOneSession(t, sessions)
	if len(results) != 2 || !strings.Contains(results[1], "MARMALADE") {
		t.Errorf("results = %q, want the second to recall MARMALADE", results)
	}
	if got := countKind(events, protocol.KindHarnessStarted); got != 2 {
		t.Errorf("harness_started %d times, want 2", got)
	}
}

// Cost: `claude --version` and two claude sessions: the three-step turn,
// cut short by the pause, and the resumed turn that finishes it.
func TestLiveGivenPauseThroughTheGUIWhenTheOwnerResumesThenANewProcessFinishesTheRemainingSteps(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	sys := startLiveSystem(t)
	task := sys.startTaskViaGUI(t, ctx, threeSteps)
	pings, pauseSent := 0, false
	allowPings := func(tool, command string) bool {
		if !allowOnlyPings(tool, command) {
			return false
		}
		if tool == "Bash" {
			pings++
		}
		return true
	}

	var firstPing time.Time
	pauseAfterFirstPing := func() {
		if pings == 0 || pauseSent {
			return
		}
		if firstPing.IsZero() {
			firstPing = time.Now()
		}
		if time.Since(firstPing) >= time.Second {
			pauseSent = true
			sys.command(t, task, url.Values{"kind": {"pause"}})
		}
	}
	paused := sys.waitForStateEach(t, ctx, task, "paused", 1, allowPings, pauseAfterFirstPing)
	sys.command(t, task, url.Values{"kind": {"resume"}})
	events := sys.waitForState(t, ctx, task, "finished", 2, allowPings)

	sessions, results := sessionsAndResults(t, events)
	requireOneSession(t, sessions)
	if countKind(paused, protocol.KindPauseAcknowledged) == 0 || countKind(paused, protocol.KindPauseSettled) != 1 {
		t.Errorf("before the resume: %d acknowledgements, %d settlements", countKind(paused, protocol.KindPauseAcknowledged), countKind(paused, protocol.KindPauseSettled))
	}
	if before, all := pingRequests(paused), pingRequests(events); before >= 3 || all != 3 {
		t.Errorf("pings: %d before the resume, %d in all; want fewer than 3, then 3", before, all)
	}
	if len(results) < 2 || !strings.Contains(results[len(results)-1], "FINISHED") {
		t.Errorf("results = %q, want the last to be FINISHED", results)
	}
	var workdirs []string
	for _, event := range events {
		if event.Kind == protocol.KindHarnessStarted {
			var started protocol.HarnessStarted
			json.Unmarshal(event.Payload, &started)
			workdirs = append(workdirs, started.Workdir)
		}
	}
	if len(workdirs) != 2 || workdirs[0] != workdirs[1] {
		t.Errorf("harness workdirs = %q, want two, the same", workdirs)
	}
}

// threeSteps asks for three ping calls, about four seconds each, so that
// a pause can arrive during the turn.
const threeSteps = "Use the Bash tool to run `ping -c 5 127.0.0.1` three times, one call after another, " +
	"never in parallel and never in the background. After each call finishes, write the single " +
	"word DONE-1, DONE-2 or DONE-3 before starting the next call. When all three are done, " +
	"reply with exactly FINISHED."

// allowOnlyPings allows the orchestrator's own tools and Bash running
// ping.
func allowOnlyPings(tool, command string) bool {
	return strings.HasPrefix(tool, "mcp__orchestrator__") || tool == "Bash" && strings.HasPrefix(command, "ping")
}

// Cost: `claude --version` and four claude sessions: a one-turn task that
// brings the first quota reading, the filler's three-step turn cut short
// by the yield, the normal one-turn task, and the filler's resumed turn.
func TestLiveGivenRunningFillerOnTheOnlySlotWhenANormalTaskArrivesThenTheFillerYieldsTheNormalTaskRunsAndTheFillerFinishes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	// A threshold of 1 lets filler run at any utilization short of the
	// whole window, so the test does not depend on the account's usage.
	sys := startLiveSystem(t, "-slots-per-daemon", "1", "-filler-threshold", "1")
	primer := sys.startTaskViaGUI(t, ctx, "Reply with exactly READY.")
	primed := sys.waitForState(t, ctx, primer, "finished", 1, nil)
	if !slices.ContainsFunc(primed, func(event protocol.Event) bool {
		return event.Kind == protocol.KindQuotaObserved && strings.Contains(string(event.Payload), `"name":"five_hour"`)
	}) {
		t.Fatalf("the first task brought no five-hour quota reading, so filler cannot run: %s", describeQuota(primed))
	}

	filler := sys.startTaskViaGUI(t, ctx, threeSteps, "filler", "on")
	var normal protocol.TaskID
	var firstPing time.Time
	startNormalAfterFirstPing := func() {
		if normal != "" || pingRequests(sys.events(t, filler)) == 0 {
			return
		}
		if firstPing.IsZero() {
			firstPing = time.Now()
		}
		if time.Since(firstPing) >= time.Second {
			normal = sys.startTaskViaGUI(t, ctx, "Reply with exactly NORMAL.")
		}
	}
	yielded := sys.waitForStateEach(t, ctx, filler, "yielded", 1, allowOnlyPings, startNormalAfterFirstPing)
	normalEvents := sys.waitForState(t, ctx, normal, "finished", 1, nil)
	fillerEvents := sys.waitForState(t, ctx, filler, "finished", 2, allowOnlyPings)

	if countKind(yielded, protocol.KindPauseSettled) != 1 {
		t.Errorf("filler settled %d pauses before it yielded, want 1", countKind(yielded, protocol.KindPauseSettled))
	}
	var note protocol.PauseAcknowledged
	for _, event := range yielded {
		if event.Kind == protocol.KindPauseAcknowledged {
			json.Unmarshal(event.Payload, &note)
		}
	}
	if note.Note == "" {
		t.Error("the filler recorded no stop note")
	} else {
		t.Logf("stop note: %q", note.Note)
	}
	var settledAt, normalStartedAt time.Time
	for _, event := range yielded {
		if event.Kind == protocol.KindPauseSettled {
			settledAt = event.Time
		}
	}
	for _, event := range normalEvents {
		if event.Kind == protocol.KindHarnessStarted {
			normalStartedAt = event.Time
		}
	}
	if normalStartedAt.Before(settledAt) {
		t.Errorf("the normal task started at %s, before the filler's pause settled at %s", normalStartedAt, settledAt)
	}
	_, results := sessionsAndResults(t, fillerEvents)
	if len(results) < 2 || !strings.Contains(results[len(results)-1], "FINISHED") {
		t.Errorf("filler results = %q, want the last to be FINISHED", results)
	}
	if before, all := pingRequests(yielded), pingRequests(fillerEvents); before >= 3 || all != 3 {
		t.Errorf("filler pings: %d before it yielded, %d in all; want fewer than 3, then 3", before, all)
	}
}

// describeQuota lists the payloads of the quota readings among events.
func describeQuota(events []protocol.Event) string {
	var readings []string
	for _, event := range events {
		if event.Kind == protocol.KindQuotaObserved {
			readings = append(readings, string(event.Payload))
		}
	}
	return fmt.Sprintf("%d readings %v", len(readings), readings)
}

// pingRequests counts the permission requests to run ping.
func pingRequests(events []protocol.Event) int {
	count := 0
	for _, event := range events {
		if event.Kind == protocol.KindPermissionRequested && strings.Contains(string(event.Payload), `"command":"ping`) {
			count++
		}
	}
	return count
}

// answerPermissions answers, through the task page's form, each
// permission request among task's events that the test has not answered
// yet: allowed when decide accepts it, denied otherwise.
func (sys liveSystem) answerPermissions(t *testing.T, task protocol.TaskID, events []protocol.Event, decide func(tool, command string) bool) {
	t.Helper()
	if sys.answeredByPolicy {
		return
	}
	for _, event := range events {
		if event.Kind != protocol.KindPermissionRequested {
			continue
		}
		var req struct {
			RequestID string `json:"request_id"`
			Tool      string `json:"tool"`
			Input     struct {
				Command string `json:"command"`
			} `json:"input"`
		}
		json.Unmarshal(event.Payload, &req)
		if sys.answered[req.RequestID] {
			continue
		}
		sys.answered[req.RequestID] = true
		decision := "deny"
		if decide != nil && decide(req.Tool, req.Input.Command) {
			decision = "allow"
		}
		t.Logf("permission %s %q: %s", req.Tool, req.Input.Command, decision)
		sys.command(t, task, url.Values{"kind": {"answer_permission"}, "request_id": {req.RequestID}, "decision": {decision},
			"message": {"Only ping is allowed in this test."}})
	}
}

// childOf returns the id of a task that parent spawned, or "" when it
// has spawned none.
func (sys liveSystem) childOf(t *testing.T, parent protocol.TaskID) protocol.TaskID {
	t.Helper()
	var tasks []struct {
		ID       protocol.TaskID  `json:"id"`
		ParentID *protocol.TaskID `json:"parent_id"`
	}
	call(t, http.MethodGet, sys.server+"/v1/tasks", nil, &tasks)
	for _, task := range tasks {
		if task.ParentID != nil && *task.ParentID == parent {
			return task.ID
		}
	}
	return ""
}

// callsTool reports whether the agent called the gateway tool name.
func callsTool(events []protocol.Event, name string) bool {
	for _, event := range events {
		if event.Kind == protocol.KindHarnessOutput && strings.Contains(string(event.Payload), `"name":"mcp__orchestrator__`+name+`"`) {
			return true
		}
	}
	return false
}

// Cost: `claude --version` and three claude sessions: the parent's turn
// that spawns the child, the child's turn that sends PEAR, and the
// parent's turn that the child's message resumes.
func TestLiveGivenParentThatSpawnsAChildWhenTheChildReportsThenTheParentIsResumedWithItsMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	sys := startLiveSystem(t)
	parent := sys.startTaskViaGUI(t, ctx, "Use the spawn_task tool once to start one child task with exactly this prompt: "+
		`"Reply with the word PEAR and send it to your parent." `+
		"Then end your turn at once, without waiting for the child or checking on it. "+
		"When the child's message arrives, reply with the word it sent.")

	first := sys.waitForState(t, ctx, parent, "finished", 1, nil)
	child := sys.childOf(t, parent)
	if child == "" {
		_, results := sessionsAndResults(t, first)
		t.Fatalf("the parent spawned no child; called spawn_task: %v; results %q", callsTool(first, "spawn_task"), results)
	}
	answerChild := func() {
		childEvents := sys.events(t, child)
		sys.answerPermissions(t, child, childEvents, nil)
		if sys.state(t, child) == "finished" && !callsTool(childEvents, "send_message") {
			_, results := sessionsAndResults(t, childEvents)
			t.Fatalf("the child finished without calling send_message; its results %q", results)
		}
	}
	events := sys.waitForStateEach(t, ctx, parent, "finished", 2, nil, answerChild)

	childEvents := sys.events(t, child)
	if !callsTool(events, "spawn_task") || !callsTool(childEvents, "send_message") {
		t.Errorf("spawn_task called: %v; send_message called by the child: %v", callsTool(events, "spawn_task"), callsTool(childEvents, "send_message"))
	}
	sessions, results := sessionsAndResults(t, events)
	requireOneSession(t, sessions)
	if len(results) < 2 || !strings.Contains(results[len(results)-1], "PEAR") {
		t.Errorf("parent's results = %q, want the last to contain PEAR", results)
	}
	resp, err := http.Get(sys.server + "/tasks/" + string(parent))
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if want := `Message from task <a href="/tasks/` + string(child) + `">`; !strings.Contains(string(page), want) {
		t.Errorf("the parent's page lacks %s", want)
	}
	_, childResults := sessionsAndResults(t, childEvents)
	t.Logf("parent %s, child %s; child's results %q", parent, child, childResults)
}

// daemonProcess is a cmd/daemon process that a test may kill or stop
// itself.
type daemonProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// runDaemon starts the system's daemon. On cleanup a daemon still
// running gets SIGTERM, and SIGKILL if it has not exited 90 s later.
func (sys liveSystem) runDaemon(t *testing.T, logs io.Writer) *daemonProcess {
	t.Helper()
	p := &daemonProcess{cmd: exec.Command(sys.daemon, sys.daemonArgs...), done: make(chan struct{})}
	p.cmd.Stderr = logs
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(90 * time.Second):
			p.cmd.Process.Kill()
			<-p.done
		}
	})
	return p
}

// signal sends sig to the daemon and returns how it exited.
func (p *daemonProcess) signal(t *testing.T, sig syscall.Signal) error {
	t.Helper()
	if err := p.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
		return p.err
	case <-time.After(90 * time.Second):
		t.Fatalf("the daemon did not exit within 90 s of %s", sig)
		return nil
	}
}

// alive reports whether a process with the pid exists.
func alive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// harnessPIDs returns the pid of every harness process among events.
func harnessPIDs(events []protocol.Event) []int {
	var pids []int
	for _, event := range events {
		if event.Kind == protocol.KindHarnessStarted {
			var started protocol.HarnessStarted
			json.Unmarshal(event.Payload, &started)
			pids = append(pids, started.PID)
		}
	}
	return pids
}

// waitUntilFirstPingRuns answers the task's permission requests, allowing
// only ping, until a second has passed since the first ping was allowed,
// so that the turn is in the middle of its first tool call. It returns
// the task's events at that moment.
func (sys liveSystem) waitUntilFirstPingRuns(t *testing.T, ctx context.Context, task protocol.TaskID) []protocol.Event {
	t.Helper()
	var allowedAt time.Time
	for {
		events := sys.events(t, task)
		sys.answerPermissions(t, task, events, allowOnlyPings)
		if allowedAt.IsZero() && pingRequests(events) > 0 {
			allowedAt = time.Now()
		}
		if !allowedAt.IsZero() && time.Since(allowedAt) >= time.Second {
			return events
		}
		if state := sys.state(t, task); slices.Contains([]string{"finished", "paused", "yielded", "failed", "stopped"}, state) {
			t.Fatalf("the task is %s before its first ping ran", state)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for the first ping: %v", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// Cost: `claude --version` twice and two claude sessions: the three-step
// turn that the daemon's death cuts short, and the resumed turn that
// finishes it. The daemon is restarted at once, while the harness the
// killed daemon started may still run; the restarted daemon must wait for
// that harness to exit, or kill it, before it reports the turn cut short
// (docs/adr/2026-10-08-shutdown-recovery.md).
func TestLiveGivenDaemonKilledMidTurnWhenItRestartsAndTheOwnerResumesThenTheOldHarnessIsGoneAndTheTurnContinuesTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	sys := newLiveSystem(t)
	logs := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", logs)
		}
	})
	first := sys.runDaemon(t, logs)
	task := sys.startTaskViaGUI(t, ctx, threeSteps)
	before := sys.waitUntilFirstPingRuns(t, ctx, task)
	pids := harnessPIDs(before)
	if len(pids) != 1 || pids[0] <= 0 {
		t.Fatalf("harness pids %v, want one", pids)
	}
	pid := pids[0]
	t.Cleanup(func() {
		if alive(pid) {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	err := first.signal(t, syscall.SIGKILL)
	killedAt := time.Now()
	aliveAtRestart := alive(pid)
	t.Logf("daemon killed: %v; harness pid %d alive at the restart: %v", err, pid, aliveAtRestart)
	sys.runDaemon(t, logs)
	paused := sys.waitForState(t, ctx, task, "paused", 1, allowOnlyPings)
	pausedAfter := time.Since(killedAt)

	if alive(pid) {
		t.Errorf("the old harness %d is still running when the task is paused", pid)
	}
	handled := regexp.MustCompile(`msg="harness left by the previous daemon (exited|did not exit; killed it)" task=` + string(task) + ` pid=` + fmt.Sprint(pid) + ` after=(\S+)`).FindStringSubmatch(logs.String())
	switch {
	case handled != nil:
		t.Logf("the restarted daemon reports the old harness %s after %s; the task was paused %s after the kill", handled[1], handled[2], pausedAfter.Round(100*time.Millisecond))
	case aliveAtRestart:
		t.Errorf("the old harness was running at the restart, and the daemon log does not say it waited for it or killed it")
	default:
		t.Logf("the old harness had exited before the restart; nothing to wait for")
	}
	for _, event := range paused {
		if event.Kind == protocol.KindHarnessExited {
			t.Logf("after the restart, seq %d harness_exited %s", event.Seq, event.Payload)
		}
	}
	sys.command(t, task, url.Values{"kind": {"resume"}})
	events := sys.waitForState(t, ctx, task, "finished", 2, allowOnlyPings)

	sessions, results := sessionsAndResults(t, events)
	requireOneSession(t, sessions)
	t.Logf("session %s; pings asked for: %d before the kill, %d in all", sessions[0], pingRequests(before), pingRequests(events))
	if len(results) == 0 || !strings.Contains(results[len(results)-1], "FINISHED") {
		t.Errorf("results = %q, want the last to be FINISHED", results)
	}
	if got := len(harnessPIDs(events)); got != 2 {
		t.Errorf("harness_started %d times, want 2", got)
	}
}

// Cost: `claude --version` twice and two claude sessions: the three-step
// turn that the daemon's clean shutdown interrupts, and the turn that the
// owner's Resume starts after the daemon restarts.
func TestLiveGivenDaemonShutDownCleanlyMidTurnWhenItRestartsAndTheOwnerResumesThenTheTaskWasPausedAndTheTurnContinuesTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	sys := newLiveSystem(t)
	logs := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", logs)
		}
	})
	first := sys.runDaemon(t, logs)
	task := sys.startTaskViaGUI(t, ctx, threeSteps)
	before := sys.waitUntilFirstPingRuns(t, ctx, task)

	stoppedAt := time.Now()
	if err := first.signal(t, syscall.SIGTERM); err != nil {
		t.Fatalf("the daemon after SIGTERM: %v", err)
	}
	t.Logf("the daemon exited %s after SIGTERM", time.Since(stoppedAt).Round(100*time.Millisecond))
	// The daemon sends a task's last events before it exits.
	var exit protocol.HarnessExited
	for _, event := range sys.events(t, task)[len(before):] {
		switch msg, err := claude.Parse(event.Payload); {
		case event.Kind == protocol.KindHarnessExited:
			t.Logf("seq %d harness_exited %s", event.Seq, event.Payload)
			json.Unmarshal(event.Payload, &exit)
		case event.Kind == protocol.KindHarnessOutput && err == nil && (msg.Type == claude.TypeResult || msg.Type == "control_response"):
			t.Logf("seq %d %s", event.Seq, event.Payload)
		}
	}
	if exit.ExitCode != -1 || exit.Error != "daemon stopped during the turn" {
		t.Errorf("harness_exited = %+v, want the daemon to say it stopped during the turn", exit)
	}
	if state := sys.state(t, task); state != "paused" {
		t.Fatalf("after the clean shutdown the task is %s, want paused", state)
	}
	if line := regexp.MustCompile(`msg="shutdown cut the turn short; the task can be resumed" task=` + string(task) + ` .*`).FindString(logs.String()); line != "" {
		t.Logf("daemon: %s", line)
	}

	sys.runDaemon(t, logs)
	sys.command(t, task, url.Values{"kind": {"resume"}})
	events := sys.waitForState(t, ctx, task, "finished", 2, allowOnlyPings)

	sessions, results := sessionsAndResults(t, events)
	requireOneSession(t, sessions)
	t.Logf("session %s; pings asked for: %d before the shutdown, %d in all", sessions[0], pingRequests(before), pingRequests(events))
	if len(results) == 0 || !strings.Contains(results[len(results)-1], "FINISHED") {
		t.Errorf("results = %q, want the last to be FINISHED", results)
	}
	if got := len(harnessPIDs(events)); got != 2 {
		t.Errorf("harness_started %d times, want 2", got)
	}
}

// Cost: `claude --version` and two claude sessions: the three-step turn
// the owner interrupts during its first ping, and the resumed turn.
func TestLiveGivenRunningTurnWhenTheOwnerInterruptsItThenTheTaskIsPausedAndResumeFinishesTheSteps(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	sys := newLiveSystem(t)
	logs := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", logs)
		}
	})
	sys.runDaemon(t, logs)
	task := sys.startTaskViaGUI(t, ctx, threeSteps)
	before := sys.waitUntilFirstPingRuns(t, ctx, task)

	sys.command(t, task, url.Values{"kind": {"interrupt"}})
	events := waitForEvents(t, ctx, sys.server+"/v1/tasks/"+string(task)+"/events", "the harness to exit", hasKind(protocol.KindHarnessExited))

	var exit protocol.HarnessExited
	for _, event := range events[len(before):] {
		switch msg, err := claude.Parse(event.Payload); {
		case event.Kind == protocol.KindHarnessExited:
			t.Logf("seq %d harness_exited %s", event.Seq, event.Payload)
			json.Unmarshal(event.Payload, &exit)
		case event.Kind == protocol.KindHarnessOutput && err == nil && (msg.Type == claude.TypeResult || msg.Type == "control_response"):
			t.Logf("seq %d %s", event.Seq, event.Payload)
		}
	}
	if line := regexp.MustCompile(`msg="the owner's interrupt cut the turn short; the task can be resumed" task=` + string(task) + ` .*`).FindString(logs.String()); line != "" {
		t.Logf("daemon: %s", line)
	}
	state := sys.state(t, task)
	t.Logf("after the interrupt the task is %s", state)
	if exit.ExitCode != -1 || exit.Error != "interrupted by the owner" {
		t.Errorf("harness_exited = %+v, want the daemon to say the owner interrupted the turn", exit)
	}
	if state != "paused" {
		t.Fatalf("after the owner's interrupt the task is %s, want paused", state)
	}
	if dashboard := getPage(t, sys.server+"/"); !strings.Contains(dashboard, "paused: interrupted by the owner") {
		t.Error("the dashboard does not give the owner's interrupt as the reason the task is paused")
	}

	sys.command(t, task, url.Values{"kind": {"resume"}})
	events = sys.waitForState(t, ctx, task, "finished", 2, allowOnlyPings)

	sessions, results := sessionsAndResults(t, events)
	requireOneSession(t, sessions)
	t.Logf("session %s; pings asked for: %d before the interrupt, %d in all", sessions[0], pingRequests(before), pingRequests(events))
	if len(results) == 0 || !strings.Contains(results[len(results)-1], "FINISHED") {
		t.Errorf("results = %q, want the last to be FINISHED", results)
	}
}

// getPage returns the page at url, failing unless it answers 200.
func getPage(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: %d, %v", url, resp.StatusCode, err)
	}
	return string(data)
}

// liveGit runs git with no configuration of the owner's and a fixed
// identity, and returns its trimmed output.
func liveGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// liveRemote makes a bare repository with one commit on main and returns
// its path and an https URL that git, in this test's processes and theirs,
// rewrites to it. The rewrite and the switch-off of commit signing, which
// the agent's git would otherwise take from the owner's configuration,
// come from the environment, which the server, the daemon and claude
// inherit.
func liveRemote(t *testing.T) (bare, repoURL string) {
	t.Helper()
	work := t.TempDir()
	liveGit(t, work, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README"), []byte("live delivery test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, work, "add", "README")
	liveGit(t, work, "commit", "--quiet", "-m", "add README")
	bare = filepath.Join(t.TempDir(), "repo.git")
	liveGit(t, "", "clone", "--quiet", "--bare", work, bare)
	prefix := "https://repos.invalid/live/"
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+filepath.Dir(bare)+"/.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", prefix)
	t.Setenv("GIT_CONFIG_KEY_1", "commit.gpgsign")
	t.Setenv("GIT_CONFIG_VALUE_1", "false")
	return bare, prefix + filepath.Base(bare)
}

// branchPushes returns the branch_pushed events among events, in order.
func branchPushes(t *testing.T, events []protocol.Event) []protocol.BranchPushed {
	t.Helper()
	var pushes []protocol.BranchPushed
	for _, event := range events {
		if event.Kind != protocol.KindBranchPushed {
			continue
		}
		var pushed protocol.BranchPushed
		if err := json.Unmarshal(event.Payload, &pushed); err != nil {
			t.Fatalf("branch_pushed %s: %v", event.Payload, err)
		}
		t.Logf("branch_pushed %+v", pushed)
		pushes = append(pushes, pushed)
	}
	return pushes
}

// Cost: `claude --version` and two claude sessions, each one turn that
// writes a file and commits it with git.
func TestLiveGivenTaskWithARepositoryWhenTheAgentCommitsEachTurnThenTheDaemonPushesItsBranchAndTheServerHearsOfIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	bare, repoURL := liveRemote(t)
	main := liveGit(t, bare, "rev-parse", "refs/heads/main")
	sys := startLiveSystem(t, "-permissions", "allow-all")
	sys.answeredByPolicy = true

	task := sys.startTaskViaGUI(t, ctx,
		"Create a file named hello.txt whose only line is: hello. Then commit it with git, with a clear commit message.",
		"repo", repoURL, "ref", "main")
	events := sys.waitForState(t, ctx, task, "finished", 1, nil)
	sys.command(t, task, url.Values{"kind": {"prompt"}, "text": {"Add a second line to hello.txt: world. Then commit that change with git."}})
	events = sys.waitForState(t, ctx, task, "finished", 2, nil)

	branch := "orchestrator/" + string(task)
	pushes := branchPushes(t, events)
	if len(pushes) != 2 {
		t.Fatalf("got %d branch_pushed events, want one per turn", len(pushes))
	}
	last := pushes[1]
	if last.Branch != branch || last.Error != "" || last.Ahead < 2 || pushes[0].Error != "" || pushes[0].Ahead < 1 {
		t.Errorf("branch_pushed = %+v, want %s pushed each turn, its commits counted", pushes, branch)
	}
	if remote := liveGit(t, bare, "rev-parse", "refs/heads/"+branch); remote != last.Commit {
		t.Errorf("remote %s = %s, want the reported %s", branch, remote, last.Commit)
	}
	if hello := liveGit(t, bare, "show", last.Commit+":hello.txt"); hello != "hello\nworld" {
		t.Errorf("hello.txt on the branch = %q, want hello and world", hello)
	}
	if got := liveGit(t, bare, "rev-parse", "refs/heads/main"); got != main {
		t.Errorf("remote main = %s, want it untouched at %s", got, main)
	}
	var detail struct {
		Branch *protocol.BranchPushed `json:"branch"`
	}
	call(t, http.MethodGet, sys.server+"/v1/tasks/"+string(task), nil, &detail)
	if detail.Branch == nil || *detail.Branch != last {
		t.Errorf("the server's task holds branch %+v, want %+v", detail.Branch, last)
	}
}
