package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

type daemonFixture struct {
	harness *fakeHarness
	// processes is the daemon's process table: no real process is looked
	// up or killed.
	processes *fakeProcesses
	stateDir  string
	cancel    context.CancelFunc
	done      chan error
}

// runDaemon serves a daemon with a fake harness until stop or the end of
// the test.
func runDaemon(t *testing.T, server *url.URL, stateDir string) *daemonFixture {
	t.Helper()
	return runDaemonWithClient(t, server, stateDir, nil)
}

// runDaemonWithClient is runDaemon reaching the server with client.
func runDaemonWithClient(t *testing.T, server *url.URL, stateDir string, client *http.Client) *daemonFixture {
	t.Helper()
	return runDaemonWithProcesses(t, server, stateDir, client, newFakeProcesses())
}

// runDaemonWithProcesses is runDaemonWithClient with procs as the
// daemon's process table from its start.
func runDaemonWithProcesses(t *testing.T, server *url.URL, stateDir string, client *http.Client, procs *fakeProcesses) *daemonFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	f := &daemonFixture{harness: newFakeHarness(), processes: procs, stateDir: stateDir, cancel: cancel, done: make(chan error, 1)}
	gateway := startTestGateway(t)
	go func() {
		f.done <- Serve(ctx, Config{
			Server:          server,
			ID:              testDaemon,
			StateDir:        stateDir,
			Harness:         f.harness,
			Gateway:         gateway,
			Log:             testLogger(t),
			Client:          client,
			MinBackoff:      5 * time.Millisecond,
			MaxBackoff:      50 * time.Millisecond,
			ShutdownTimeout: time.Second,
			processes:       f.processes,
		})
	}()
	t.Cleanup(func() { f.stop(t) })
	return f
}

// stop shuts the daemon down and waits for Serve to return.
func (f *daemonFixture) stop(t *testing.T) {
	t.Helper()
	f.cancel()
	if err, ok := <-f.done; ok {
		close(f.done)
		if err != nil {
			t.Errorf("serve: %v", err)
		}
	}
}

func (f *daemonFixture) nextProcess(t *testing.T) *fakeProcess {
	t.Helper()
	select {
	case p := <-f.harness.started:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no harness started")
		return nil
	}
}

// tcpProxy forwards connections to a target and can cut them all and
// refuse new ones until restored.
type tcpProxy struct {
	listener net.Listener
	target   string

	mu    sync.Mutex
	cut   bool
	conns []net.Conn
}

func startProxy(t *testing.T, target *url.URL) *tcpProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &tcpProxy{listener: listener, target: target.Host}
	go p.serve()
	t.Cleanup(func() {
		listener.Close()
		p.cutAll()
	})
	return p
}

func (p *tcpProxy) url() *url.URL {
	return &url.URL{Scheme: "http", Host: p.listener.Addr().String()}
}

func (p *tcpProxy) serve() {
	for {
		down, err := p.listener.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		if p.cut {
			p.mu.Unlock()
			down.Close()
			continue
		}
		up, err := net.Dial("tcp", p.target)
		if err != nil {
			p.mu.Unlock()
			down.Close()
			continue
		}
		p.conns = append(p.conns, down, up)
		p.mu.Unlock()
		go func() {
			io.Copy(up, down)
			up.Close()
			down.Close()
		}()
		go func() {
			io.Copy(down, up)
			up.Close()
			down.Close()
		}()
	}
}

// cutAll closes every connection and refuses new ones.
func (p *tcpProxy) cutAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = true
	for _, conn := range p.conns {
		conn.Close()
	}
	p.conns = nil
}

func (p *tcpProxy) restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cut = false
}

func isKind(kind protocol.Kind) func(protocol.Event) bool {
	return func(event protocol.Event) bool { return event.Kind == kind }
}

func hasSeq(seq uint64) func(protocol.Event) bool {
	return func(event protocol.Event) bool { return event.Seq == seq }
}

func acknowledgePauseThroughGateway(t *testing.T, proc *fakeProcess, note string) {
	t.Helper()
	session := mustConnect(t, proc.spec.Gateway.URL)
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: AcknowledgePauseTool, Arguments: map[string]any{"note": note}})
	if err != nil || result.IsError {
		t.Fatalf("acknowledge_pause: %v %+v", err, result)
	}
}

func TestGivenConnectedDaemonWhenTheOwnerCreatesPromptsAndPausesATaskThenTheHarnessGetsEachAndTheServerGetsItsEvents(t *testing.T) {
	for name, start := range map[string]func(*testing.T) *serverFixture{
		"plain HTTP without authentication": startServer,
		"mutual TLS":                        startTLSServer,
	} {
		t.Run(name, func(t *testing.T) {
			srv := start(t)
			d := runDaemonWithClient(t, srv.url, t.TempDir(), srv.daemon)
			ownerCreatesPromptsAndPausesATask(t, srv, d)
		})
	}
}

func ownerCreatesPromptsAndPausesATask(t *testing.T, srv *serverFixture, d *daemonFixture) {
	t.Helper()

	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", Model: "fake-model", PauseLimits: testPauseLimits})

	proc := d.nextProcess(t)
	if want := workspacePath(d.stateDir, task); proc.spec.Workdir != want || proc.spec.Model != "fake-model" {
		t.Errorf("spec = %+v, want workdir %s", proc.spec, want)
	}
	if info, err := os.Stat(proc.spec.Workdir); err != nil || !info.IsDir() {
		t.Errorf("workspace: %v", err)
	}
	if in := proc.nextInput(t); in.kind != "prompt" || in.text != "Do the work." {
		t.Errorf("first input = %+v", in)
	}
	proc.emit(harness.Output{Line: []byte(`{"type":"assistant","step":1}`)})
	srv.waitForEvent(t, task, "the harness output", isKind(protocol.KindHarnessOutput))

	srv.command(t, task, protocol.CommandPrompt, protocol.Prompt{Text: "Also this."})
	if in := proc.nextInput(t); in.kind != "prompt" || in.text != "Also this." {
		t.Errorf("follow-up input = %+v", in)
	}
	srv.command(t, task, protocol.CommandPause, nil)
	if in := proc.nextInput(t); in.kind != "prompt" || in.text != pausePrompt {
		t.Errorf("pause input = %+v", in)
	}
	acknowledgePauseThroughGateway(t, proc, "Stopped after step 1.")

	events := srv.waitForEvent(t, task, "pause_acknowledged", isKind(protocol.KindPauseAcknowledged))
	assertContiguous(t, events)
	if events[0].Kind != protocol.KindHarnessStarted || string(events[1].Payload) != `{"type":"assistant","step":1}` {
		t.Errorf("events: %s", describe(events))
	}
	if last := events[len(events)-1]; string(last.Payload) != `{"note":"Stopped after step 1."}` {
		t.Errorf("pause_acknowledged payload = %s", last.Payload)
	}
}

func TestGivenLostConnectionWhenItIsRestoredThenTheServerHasEveryEventOnceAndThePauseIssuedMeanwhileIsAppliedOnce(t *testing.T) {
	srv := startServer(t)
	proxy := startProxy(t, srv.url)
	d := runDaemon(t, proxy.url(), t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)
	proc.nextInput(t)
	proc.emit(harness.Output{Line: []byte(`{"n":1}`)})
	srv.waitForEvent(t, task, "seq 2", hasSeq(2))

	proxy.cutAll()
	srv.command(t, task, protocol.CommandPause, nil)
	proc.emit(harness.Output{Line: []byte(`{"n":2}`)})
	proc.emit(harness.Output{Line: []byte(`{"n":3}`)})
	eventually(t, "the journal to grow", func() bool {
		return len(readJournalFile(t, JournalPath(d.stateDir, task))) == 4
	})
	if held := srv.events(t, task); len(held) != 2 {
		t.Fatalf("server got events through a cut connection: %s", describe(held))
	}
	proc.noInput(t)

	proxy.restore()

	if in := proc.nextInput(t); in.kind != "prompt" || in.text != pausePrompt {
		t.Errorf("input after reconnecting = %+v, want the pause request", in)
	}
	srv.waitForEvent(t, task, "seq 4", hasSeq(4))
	// A second outage makes the daemon reconnect again; the pause must
	// not come back. The interrupt issued after it shows that every
	// earlier command has been through the daemon.
	proxy.cutAll()
	proxy.restore()
	srv.command(t, task, protocol.CommandInterrupt, nil)
	if in := proc.nextInput(t); in.kind != "interrupt" {
		t.Errorf("input = %+v, want only the interrupt after the pause", in)
	}
	proc.noInput(t)
	events := srv.events(t, task)
	assertContiguous(t, events)
	if len(events) != 4 {
		t.Errorf("events: %s", describe(events))
	}
	for idx, want := range []string{`{"n":1}`, `{"n":2}`, `{"n":3}`} {
		if got := string(events[idx+1].Payload); got != want {
			t.Errorf("seq %d payload = %s, want %s", idx+2, got, want)
		}
	}
}

func TestGivenDaemonThatDiedWithATaskRunningWhenANewOneStartsOnItsStateThenTheTaskIsPausedAndRunsAgainOnlyWhenResumed(t *testing.T) {
	srv := startServer(t)
	proxy := startProxy(t, srv.url)
	first := runDaemon(t, proxy.url(), t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := first.nextProcess(t)
	proc.nextInput(t)
	proc.emit(harness.Output{Line: []byte(`{"n":1}`)})
	srv.waitForEvent(t, task, "seq 2", hasSeq(2))
	// What a crash leaves on disk: the state directory as it is while the
	// task runs. The first daemon is then cut off and stopped, so nothing
	// it does after this point reaches the server.
	crashed := t.TempDir()
	if err := os.CopyFS(crashed, os.DirFS(first.stateDir)); err != nil {
		t.Fatal(err)
	}
	proxy.cutAll()
	first.stop(t)
	pause := srv.command(t, task, protocol.CommandPause, nil)

	second := runDaemon(t, srv.url, crashed)

	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	assertContiguous(t, events)
	var exit protocol.HarnessExited
	json.Unmarshal(events[len(events)-1].Payload, &exit)
	if len(events) != 3 || exit.ExitCode != -1 || exit.Error != restartNoSessionError {
		t.Errorf("events: %s", describe(events))
	}
	srv.waitForState(t, task, "paused")
	next := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Next.", PauseLimits: testPauseLimits})
	if proc := second.nextProcess(t); !strings.HasSuffix(proc.spec.Workdir, string(next)) {
		t.Errorf("first harness the new daemon started is in %s, want the new task's workspace", proc.spec.Workdir)
	}
	if len(second.harness.started) != 0 {
		t.Error("the new daemon started another harness")
	}
	if got := mustLoadState(t, crashed).lastCommand(); got <= pause.ID {
		t.Errorf("last command = %d, want past the pause %d", got, pause.ID)
	}

	srv.command(t, task, protocol.CommandResume, nil)

	// No session was recorded, so the task starts again in a new one.
	resumed := second.nextProcess(t)
	if resumed.spec.Resume != "" || !strings.HasSuffix(resumed.spec.Workdir, string(task)) {
		t.Errorf("resumed spec = %+v, want a new session in the task's workspace", resumed.spec)
	}
	if in := resumed.nextInput(t); in.text != "Do the work." {
		t.Errorf("prompt = %q, want the task's first prompt", in.text)
	}
}

func TestGivenTaskWithAWorkspaceWhenItStartsThenTheHarnessWorksInTheClone(t *testing.T) {
	repo := makeTestRepo(t)
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())

	srv.createTask(t, testDaemon, protocol.StartTask{
		Prompt: "Do the work.", Workspace: &protocol.Workspace{Repo: repo.url, Ref: "feature"}, PauseLimits: testPauseLimits,
	})

	proc := d.nextProcess(t)
	if got := readWorkspaceFile(t, proc.spec.Workdir, "feature.txt"); got != "feature" {
		t.Errorf("feature.txt = %q", got)
	}
}

func TestGivenWorkspaceThatCannotBeClonedWhenTheTaskStartsThenItEndsWithGitsErrorAndNoHarness(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	missing := httpsAlias(t, filepath.Join(t.TempDir(), "missing.git"))

	task := srv.createTask(t, testDaemon, protocol.StartTask{
		Prompt: "Do the work.", Workspace: &protocol.Workspace{Repo: missing, Ref: "main"}, PauseLimits: testPauseLimits,
	})

	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	var exit protocol.HarnessExited
	json.Unmarshal(events[0].Payload, &exit)
	if len(events) != 1 || exit.ExitCode != -1 || !strings.Contains(exit.Error, "git clone") || !strings.Contains(exit.Error, "missing.git") {
		t.Errorf("events: %s", describe(events))
	}
	if len(d.harness.started) != 0 {
		t.Error("a harness started")
	}
}

func TestGivenRepositoryThatIsNotAnHTTPSURLWhenTheTaskStartsThenItEndsSayingSoAndNoHarnessStarts(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())

	task := srv.createTask(t, testDaemon, protocol.StartTask{
		Prompt: "Do the work.", Workspace: &protocol.Workspace{Repo: "git@github.com:octocat/Hello-World.git", Ref: "master"}, PauseLimits: testPauseLimits,
	})

	events := srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	var exit protocol.HarnessExited
	json.Unmarshal(events[0].Payload, &exit)
	want := `prepare workspace: repository "git@github.com:octocat/Hello-World.git" is not an https:// URL`
	if len(events) != 1 || exit.ExitCode != -1 || !strings.HasPrefix(exit.Error, want) {
		t.Errorf("events: %s", describe(events))
	}
	if len(d.harness.started) != 0 {
		t.Error("a harness started")
	}
}
