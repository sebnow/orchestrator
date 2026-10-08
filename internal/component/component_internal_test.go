package component

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

func render(t *testing.T, node html.Node) string {
	t.Helper()
	var out strings.Builder
	if err := html.Render(&out, node); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestGivenEachTaskStateWhenMappedToABadgeThenItHasItsOwnClassAndLabel(t *testing.T) {
	for _, tc := range []struct{ state, class, label string }{
		{"pending", "state-pending", "pending"},
		{"running", "state-running", "running"},
		{"awaiting_permission", "state-awaiting", "awaiting permission"},
		{"pausing", "state-pausing", "pausing"},
		{"paused", "state-paused", "paused"},
		{"finished", "state-finished", "finished"},
		{"stopped", "state-stopped", "stopped"},
		{"failed", "state-failed", "failed"},
		{"exploded", "state-unknown", "exploded"},
	} {
		class, label := stateBadge(tc.state)
		if class != tc.class || label != tc.label {
			t.Errorf("stateBadge(%q) = %q, %q; want %q, %q", tc.state, class, label, tc.class, tc.label)
		}
	}
}

func TestGivenHostileStateWhenBadgeRenderedThenTheClassAndLabelAreEscaped(t *testing.T) {
	got := render(t, StateBadge(`"><script>x</script>`))

	want := `<span class="badge state-unknown">&#34;&gt;&lt;script&gt;x&lt;/script&gt;</span>`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenAPermissionDecisionWhenMappedToAButtonThenAllowIsPrimaryAndDenyIsDanger(t *testing.T) {
	if got := decisionVariant(true); got != VariantPrimary {
		t.Errorf("allow = %q, want %q", got, VariantPrimary)
	}
	if got := decisionVariant(false); got != VariantDanger {
		t.Errorf("deny = %q, want %q", got, VariantDanger)
	}
}

const hostile = `<script>alert("pwned")</script>`

func TestGivenEntriesCarryingScriptWhenRenderedThenNoScriptElementReachesThePage(t *testing.T) {
	bodies := []transcript.Body{
		transcript.OwnerPrompt{Text: hostile},
		transcript.PermissionAnswered{RequestID: hostile, Message: hostile},
		transcript.AgentText{Text: hostile},
		transcript.AgentThinking{Text: hostile},
		transcript.ToolCall{ID: hostile, Name: hostile, Input: json.RawMessage(`{"command":"` + strings.ReplaceAll(hostile, `"`, `\"`) + `"}`)},
		transcript.ToolResult{Content: hostile, IsError: true},
		transcript.TurnEnded{Outcome: hostile, StopReason: hostile},
		transcript.PermissionRequested{Tool: hostile, Input: json.RawMessage(hostile)},
		transcript.PauseAcknowledged{Note: hostile},
		transcript.HarnessStarted{Model: hostile, Workdir: hostile},
		transcript.HarnessExited{ExitCode: 1, Error: hostile, Stderr: hostile},
		transcript.Unknown{RecordKind: hostile, Type: hostile, Raw: json.RawMessage(hostile)},
		transcript.MessageSent{To: "task-2", Text: hostile},
		transcript.MessageReceived{Text: hostile},
		transcript.ChildSpawned{Child: "task-2", Prompt: hostile},
	}
	for _, body := range bodies {
		t.Run(string(body.Kind()), func(t *testing.T) {
			got := render(t, TranscriptEntry(transcript.Entry{Time: time.Now(), Body: body}))

			if strings.Contains(got, "<script") {
				t.Fatalf("unescaped script in %s", got)
			}
			if !strings.Contains(got, "&lt;script&gt;alert(") {
				t.Fatalf("hostile text missing or mangled in %s", got)
			}
		})
	}
}

func TestGivenAgentTextWhenRenderedThenItsWhitespaceIsKeptInAPreformattedBlock(t *testing.T) {
	got := render(t, TranscriptEntry(transcript.Entry{Body: transcript.AgentText{Text: "\n  *not* markdown\n\tindented"}}))

	if want := "<pre>\n\n  *not* markdown\n\tindented</pre>"; !strings.Contains(got, want) {
		t.Errorf("got %s\nwant it to contain %q", got, want)
	}
}

func TestGivenToolCallWhenRenderedThenItsInputIsPrettyPrintedInsideDetails(t *testing.T) {
	got := render(t, TranscriptEntry(transcript.Entry{Body: transcript.ToolCall{Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}}))

	want := "<details><summary>Input</summary><pre>\n{\n  &#34;command&#34;: &#34;ls&#34;\n}</pre></details>"
	if !strings.Contains(got, want) || !strings.Contains(got, "Agent called Bash") {
		t.Errorf("got %s\nwant it to contain %q", got, want)
	}
}

func TestGivenNoControlsOfferedWhenRenderedThenThereIsNoForm(t *testing.T) {
	if got := render(t, html.Fragment(Controls("task-1", ControlSet{}))); got != "" {
		t.Errorf("got %s, want nothing", got)
	}
}

func TestGivenNewTaskFormWhenRenderedThenTheRepositoryFieldSaysOnlyHTTPSIsAccepted(t *testing.T) {
	got := render(t, NewTaskForm(NewTask{}, []string{"laptop"}, "haiku", "", ""))

	if !strings.Contains(got, "Repository (https:// only)") {
		t.Errorf("form lacks the https note: %s", got)
	}
}

func TestGivenMessagesAndChildrenWhenRenderedThenEachSaysWhatHappenedAndLinksTheOtherTask(t *testing.T) {
	child := protocol.TaskID("child-1")
	for _, tc := range []struct {
		body transcript.Body
		want string
	}{
		{transcript.MessageSent{To: child, Text: "PEAR"},
			`<li class="entry from-agent"><header>never Agent sent a message to task <a href="/tasks/child-1">child-1</a></header><pre>`+"\n"+`PEAR</pre></li>`},
		{transcript.MessageReceived{From: &child, Text: "PEAR"},
			`<li class="entry from-task"><header>never Message from task <a href="/tasks/child-1">child-1</a></header><pre>`+"\n"+`PEAR</pre></li>`},
		{transcript.MessageReceived{Text: "Your child task child-1 has ended as failed."},
			`<li class="entry from-task"><header>never Notice from the orchestrator</header><pre>`+"\n"+`Your child task child-1 has ended as failed.</pre></li>`},
		{transcript.ChildSpawned{Child: child, Prompt: "Say PEAR."},
			`<li class="entry from-agent"><header>never Agent started child task <a href="/tasks/child-1">child-1</a></header><pre>`+"\n"+`Say PEAR.</pre></li>`},
		{transcript.ChildEnded{Child: child, State: "failed"},
			`<li class="entry from-daemon"><header>never Child task <a href="/tasks/child-1">child-1</a> ended as failed</header></li>`},
		{transcript.MessageUndeliverable{To: child, State: "stopped"},
			`<li class="entry from-daemon"><header>never Task <a href="/tasks/child-1">child-1</a> ended as stopped before the agent&#39;s messages reached it; they were not delivered</header></li>`},
	} {
		if got := render(t, TranscriptEntry(transcript.Entry{Body: tc.body})); got != tc.want {
			t.Errorf("%s:\ngot  %s\nwant %s", tc.body.Kind(), got, tc.want)
		}
	}
}

func TestGivenSpawnedTaskWhenShownThenItsRowMarksItsParentAndItsHeaderLinksParentAndChildren(t *testing.T) {
	task := Task{ID: "child-1", State: "running", Prompt: "Say PEAR.", Parent: "parent-1", Children: []string{"grandchild-1", "grandchild-2"}}

	row := render(t, TaskRow(task))
	header := render(t, TaskHeader(task, nil))

	if want := `<a href="/tasks/child-1">Say PEAR.</a><span class="reason"> ↳ child of <a href="/tasks/parent-1">parent-1</a></span>`; !strings.Contains(row, want) {
		t.Errorf("row %s\nlacks %s", row, want)
	}
	for _, want := range []string{
		`<dt>Parent</dt><dd><a href="/tasks/parent-1">parent-1</a></dd>`,
		`<dt>Children</dt><dd><a href="/tasks/grandchild-1">grandchild-1</a>, <a href="/tasks/grandchild-2">grandchild-2</a></dd>`,
	} {
		if !strings.Contains(header, want) {
			t.Errorf("header %s\nlacks %s", header, want)
		}
	}
	if owners := render(t, TaskHeader(Task{ID: "task-1"}, nil)); strings.Contains(owners, "Parent") || strings.Contains(owners, "Children") {
		t.Errorf("an owner's task without children shows family: %s", owners)
	}
}
