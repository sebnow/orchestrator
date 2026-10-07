//go:build live

// The live test builds cmd/server and cmd/daemon, runs them, and has the
// daemon run the real claude CLI on PATH with the model haiku, under
// whatever login the machine has. It spends subscription quota: the
// daemon runs `claude --version` once and one claude session. Run with:
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

var serving = regexp.MustCompile(`msg=serving address=(\S+)`)

// startServer runs cmd/server on a free loopback port and returns its
// base URL, read from its log.
func startServer(t *testing.T, bin string) string {
	t.Helper()
	reader, writer := io.Pipe()
	logs := &syncBuffer{}
	startProcess(t, filepath.Join(bin, "server"), writer, "-listen", "127.0.0.1:0", "-db", filepath.Join(t.TempDir(), "server.db"))
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
		"-server", server, "-id", "live-daemon", "-state-dir", t.TempDir())
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
