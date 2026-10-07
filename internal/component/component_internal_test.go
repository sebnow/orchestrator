package component

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/html"
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
