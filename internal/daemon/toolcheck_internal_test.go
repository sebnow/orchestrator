package daemon

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// lastExit decodes the journal's last harness_exited.
func lastExit(t *testing.T, events []protocol.Event) protocol.HarnessExited {
	t.Helper()
	var exit protocol.HarnessExited
	for _, event := range events {
		if event.Kind == protocol.KindHarnessExited {
			if err := json.Unmarshal(event.Payload, &exit); err != nil {
				t.Fatal(err)
			}
		}
	}
	return exit
}

func TestGivenRestrictedTaskWhoseHarnessOffersAnUnclassifiedToolWhenItStartsThenItIsInterruptedAndExitsNamingTheTool(t *testing.T) {
	spec := testTaskSpec
	spec.ToolClasses = []string{protocol.ToolClassRead}
	f := startTestTask(t, startTestGateway(t), spec)
	if in := f.proc.nextInput(t); in.kind != "prompt" {
		t.Fatalf("first input = %+v, want the prompt", in)
	}

	f.proc.emit(harness.Output{Line: []byte(`{"type":"system","subtype":"init"}`),
		Tools: &harness.ToolCheck{Restricted: true, Unclassified: []string{"Monitor"}}})

	if in := f.proc.nextInput(t); in.kind != "interrupt" {
		t.Errorf("input = %+v, want an interrupt", in)
	}
	if in := f.proc.nextInput(t); in.kind != "close" {
		t.Errorf("input = %+v, want the input closed", in)
	}
	if err := f.task.Prompt("Go on."); err == nil {
		t.Error("a prompt after the refusal was taken")
	}
	f.end(t, protocol.HarnessExited{ExitCode: 1})
	exit := lastExit(t, f.journal(t))
	want := "harness tool Monitor is not classified; the agent's tool restriction cannot be enforced"
	if exit.ExitCode != -1 || exit.Error != want {
		t.Errorf("exit = %+v, want -1 with %q", exit, want)
	}
}

func TestGivenRestrictedTaskOfferedToolsOfAClassItLacksWhenItStartsThenTheExitNamesThemToo(t *testing.T) {
	got := unenforceable(harness.ToolCheck{Restricted: true, Unclassified: []string{"Monitor", "Skill"}, Excess: []string{"Bash"}})

	want := "harness tools Monitor, Skill are not classified, and harness tool Bash is of a class the agent is not allowed, yet offered; " +
		"the agent's tool restriction cannot be enforced"
	if got != want {
		t.Errorf("text = %q\nwant %q", got, want)
	}
}

func TestGivenUnrestrictedTaskWhoseHarnessOffersAnUnclassifiedToolWhenItStartsThenItRunsAndTheDaemonHearsOfTheTool(t *testing.T) {
	h := newFakeHarness()
	d := New(t.TempDir(), h, startTestGateway(t), nil)
	var heard []harness.ToolCheck
	d.toolsChecked = func(task protocol.TaskID, check harness.ToolCheck) { heard = append(heard, check) }
	task, err := d.StartTask(t.Context(), testTaskSpec)
	if err != nil {
		t.Fatal(err)
	}
	proc := <-h.started
	prompt := proc.nextInput(t)

	proc.emit(harness.Output{Line: []byte(`{"type":"system","subtype":"init"}`), Tools: &harness.ToolCheck{Unclassified: []string{"Monitor"}}})
	proc.emit(harness.Output{Line: []byte(`{"type":"result"}`), TurnEnded: true, Answering: []string{prompt.id}})

	if in := proc.nextInput(t); in.kind != "close" {
		t.Errorf("input = %+v, want the input closed once the turn ended, not an interrupt", in)
	}
	proc.end(protocol.HarnessExited{})
	<-task.Done()
	if exit := task.State().Exit; exit == nil || exit.ExitCode != 0 || exit.Error != "" {
		t.Errorf("exit = %+v, want the harness's own clean exit", exit)
	}
	if len(heard) != 1 || !slices.Equal(heard[0].Unclassified, []string{"Monitor"}) {
		t.Errorf("daemon heard %+v, want the unclassified tool once", heard)
	}
}

func TestGivenTaskWithToolClassesWhenItStartsThenTheHarnessGetsThem(t *testing.T) {
	spec := testTaskSpec
	spec.ToolClasses = []string{protocol.ToolClassRead, protocol.ToolClassWeb}
	f := startTestTask(t, startTestGateway(t), spec)
	defer f.end(t, protocol.HarnessExited{})

	if got := f.proc.spec.ToolClasses; !slices.Equal(got, spec.ToolClasses) {
		t.Errorf("harness tool classes = %q, want %q", got, spec.ToolClasses)
	}
}
