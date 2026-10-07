package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// fakeHarness is an in-memory harness: tests write its output and read
// what the daemon sent to its input.
type fakeHarness struct {
	started  chan *fakeProcess
	startErr error
}

func newFakeHarness() *fakeHarness {
	return &fakeHarness{started: make(chan *fakeProcess, 4)}
}

func (h *fakeHarness) Info() protocol.Harness {
	return protocol.Harness{Name: "fake", Version: "1.0"}
}

func (h *fakeHarness) Start(ctx context.Context, spec harness.Spec) (harness.Process, error) {
	if h.startErr != nil {
		return nil, h.startErr
	}
	p := &fakeProcess{
		spec:       spec,
		out:        make(chan harness.Output),
		in:         make(chan fakeInput, 64),
		exit:       make(chan protocol.HarnessExited, 1),
		endOnClose: true,
	}
	h.started <- p
	return p, nil
}

// ParsePermission takes {"tool": ..., "input": ...}.
func (h *fakeHarness) ParsePermission(arguments json.RawMessage) (harness.PermissionRequest, error) {
	var args struct {
		Tool  string          `json:"tool"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil || args.Tool == "" {
		return harness.PermissionRequest{}, errors.New("fake: bad permission arguments")
	}
	return harness.PermissionRequest{Tool: args.Tool, Input: args.Input}, nil
}

func (h *fakeHarness) EncodeDecision(req harness.PermissionRequest, decision harness.Decision) string {
	if decision.Allow {
		return "allow " + req.Tool
	}
	return "deny: " + decision.Message
}

type fakeInput struct {
	kind string // "prompt", "interrupt" or "close"
	id   string
	text string
}

type fakeProcess struct {
	spec harness.Spec
	out  chan harness.Output
	in   chan fakeInput
	exit chan protocol.HarnessExited
	// endOnClose makes CloseInput end the process with code 0, as Claude
	// Code does when stdin closes.
	endOnClose bool
	endOnce    sync.Once
}

func (p *fakeProcess) PID() int { return 4242 }

func (p *fakeProcess) Read() (harness.Output, error) {
	out, ok := <-p.out
	if !ok {
		return harness.Output{}, io.EOF
	}
	return out, nil
}

func (p *fakeProcess) Prompt(id, text string) error {
	p.in <- fakeInput{kind: "prompt", id: id, text: text}
	return nil
}

func (p *fakeProcess) Interrupt() error {
	p.in <- fakeInput{kind: "interrupt"}
	return nil
}

func (p *fakeProcess) CloseInput() error {
	p.in <- fakeInput{kind: "close"}
	if p.endOnClose {
		p.end(protocol.HarnessExited{ExitCode: 0})
	}
	return nil
}

func (p *fakeProcess) Kill() error {
	p.end(protocol.HarnessExited{ExitCode: -1, Error: "signal: killed"})
	return nil
}

func (p *fakeProcess) Wait() protocol.HarnessExited {
	return <-p.exit
}

// end makes the process exit: Read returns EOF and Wait returns exit.
func (p *fakeProcess) end(exit protocol.HarnessExited) {
	p.endOnce.Do(func() {
		p.exit <- exit
		close(p.out)
	})
}

func (p *fakeProcess) emit(out harness.Output) {
	p.out <- out
}

func (p *fakeProcess) nextInput(t *testing.T) fakeInput {
	t.Helper()
	select {
	case in := <-p.in:
		return in
	case <-t.Context().Done():
		t.Fatal("no input before the test ended")
		return fakeInput{}
	}
}

func (p *fakeProcess) noInput(t *testing.T) {
	t.Helper()
	select {
	case in := <-p.in:
		t.Errorf("unexpected input %+v", in)
	default:
	}
}
