package component

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/transcript"
)

// subagentRun is a main conversation that starts a subagent with the tool
// call "call-1", and the subagent's own entries.
var subagentRun = []transcript.Entry{
	{Source: transcript.Source{Seq: 1}, Body: transcript.ToolCall{ID: "call-1", Name: "Agent", Input: json.RawMessage(`{"description":"Count the files"}`)}},
	{Source: transcript.Source{Seq: 2}, Body: transcript.AgentText{Text: "SUB-ONE", ParentToolUseID: "call-1"}},
	{Source: transcript.Source{Seq: 3}, Body: transcript.ToolCall{ID: "call-2", Name: "Bash", Input: json.RawMessage(`{}`), ParentToolUseID: "call-1"}},
	{Source: transcript.Source{Seq: 4}, Body: transcript.ToolResult{ToolCallID: "call-2", Content: "3", ParentToolUseID: "call-1"}},
	{Source: transcript.Source{Seq: 5}, Body: transcript.AgentText{Text: "SUB-TWO", ParentToolUseID: "call-1"}},
	{Source: transcript.Source{Seq: 6}, Body: transcript.AgentText{Text: "MAIN"}},
}

func TestGivenSubagentEntriesWhenTheTranscriptIsRenderedThenTheyAreNestedUnderTheSpawningCallTitledByIt(t *testing.T) {
	got := render(t, Transcript(subagentRun))

	list := `<ol id="subagent-` + "63616c6c2d31" + `" class="transcript">`
	call := strings.Index(got, "Agent called Agent")
	summary := strings.Index(got, `<details class="subagent" open=""><summary>Subagent started by Agent: Count the files</summary>`+list)
	one, two, main := strings.Index(got, "SUB-ONE"), strings.Index(got, "SUB-TWO"), strings.Index(got, "MAIN")
	end := strings.LastIndex(got, "</ol></details></li>")
	if call < 0 || summary < call || one < summary || two < one || end < two || main < end {
		t.Errorf("subagent entries are not nested under their call in order:\n%s", got)
	}
}

func TestGivenSubagentEntriesArriveAfterTheirCallIsShownWhenRenderedThenTheyAreAppendedToItsListOutOfBand(t *testing.T) {
	got := render(t, TranscriptEntries(subagentRun[4:], subagentRun[:4]))

	want := `<ol hx-swap-oob="beforeend:#subagent-63616c6c2d31"><li class="entry from-agent">`
	if !strings.Contains(got, want) || strings.Index(got, "SUB-TWO") < strings.Index(got, want) {
		t.Errorf("got %s\nwant SUB-TWO appended out of band with %s", got, want)
	}
	if strings.Index(got, "MAIN") > strings.Index(got, want) {
		t.Errorf("the main conversation's entry went into the out-of-band list: %s", got)
	}
}

func TestGivenSubagentEntriesWhoseCallIsNowhereWhenRenderedThenTheyAreGroupedUnderTheCallsID(t *testing.T) {
	got := render(t, Transcript(subagentRun[1:]))

	if !strings.Contains(got, "<summary>Subagent of tool call call-1</summary>") || strings.Count(got, "SUB-ONE") != 1 {
		t.Errorf("orphaned subagent entries are not grouped once: %s", got)
	}
}
