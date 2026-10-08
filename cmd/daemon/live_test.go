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

// startServer runs cmd/server on a free loopback port and returns its
// base URL, read from its log.
func startServer(t *testing.T, bin string) string {
	t.Helper()
	reader, writer := io.Pipe()
	logs := &syncBuffer{}
	startProcess(t, filepath.Join(bin, "server"), writer, "-insecure-loopback", "-listen", "127.0.0.1:0", "-db", filepath.Join(t.TempDir(), "server.db"))
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
	// answered holds the permission requests the test has answered.
	answered map[string]bool
}

func startLiveSystem(t *testing.T) liveSystem {
	t.Helper()
	bin := buildBinaries(t)
	sys := liveSystem{server: startServer(t, bin), invocations: filepath.Join(t.TempDir(), "invocations.log"), answered: map[string]bool{}}
	wrapper := filepath.Join(bin, "claude-counting")
	// Only the first argument is logged: the others include the system
	// prompt, whose line breaks would count as further invocations.
	script := "#!/bin/sh\nprintf '%s\\n' \"$1\" >> '" + sys.invocations + "'\nexec claude \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	daemonLogs := &syncBuffer{}
	startProcess(t, filepath.Join(bin, "daemon"), daemonLogs,
		append([]string{"-server", sys.server, "-state-dir", t.TempDir(), "-claude", wrapper}, daemonCredentials(t)...)...)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon log:\n%s", daemonLogs)
		}
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
// has connected.
func (sys liveSystem) startTaskViaGUI(t *testing.T, ctx context.Context, prompt string) protocol.TaskID {
	t.Helper()
	form := url.Values{"prompt": {prompt}, "repo": {""}, "ref": {""}, "model": {"haiku"}, "daemon": {"live-daemon"},
		"acknowledge": {"90s"}, "cleanup": {"1m"}}
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
	const threeSteps = "Use the Bash tool to run `ping -c 5 127.0.0.1` three times, one call after another, " +
		"never in parallel and never in the background. After each call finishes, write the single " +
		"word DONE-1, DONE-2 or DONE-3 before starting the next call. When all three are done, " +
		"reply with exactly FINISHED."
	task := sys.startTaskViaGUI(t, ctx, threeSteps)
	pings, pauseSent := 0, false
	allowPings := func(tool, command string) bool {
		if strings.HasPrefix(tool, "mcp__orchestrator__") {
			return true
		}
		if tool != "Bash" || !strings.HasPrefix(command, "ping") {
			return false
		}
		pings++
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
