package claude_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/harness/claude"
)

// replay has the fake write lines to stdout and returns what Read makes of
// them.
func replay(t *testing.T, lines [][]byte) []harness.Output {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(path, append(bytes.Join(lines, []byte("\n")), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CLAUDE_REPLAY", path)
	spec := testSpec
	spec.Workdir = t.TempDir()
	proc, err := fakeHarness(t).Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { proc.Kill() })
	var outputs []harness.Output
	for range lines {
		outputs = append(outputs, read(t, proc))
	}
	if err := proc.CloseInput(); err != nil {
		t.Fatal(err)
	}
	if _, err := proc.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("read after the replay = %v, want EOF", err)
	}
	proc.Wait()
	return outputs
}

// turnEnds returns the indices of the outputs that end a turn.
func turnEnds(outputs []harness.Output) []int {
	var ends []int
	for idx, out := range outputs {
		if out.TurnEnded {
			ends = append(ends, idx)
		}
	}
	return ends
}

// resultIndices returns the indices of the result lines.
func resultIndices(t *testing.T, lines [][]byte) []int {
	t.Helper()
	var indices []int
	for idx, line := range lines {
		msg, err := claude.Parse(line)
		if err != nil {
			t.Fatalf("line %d: %v", idx+1, err)
		}
		if _, ok := msg.Result(); ok {
			indices = append(indices, idx)
		}
	}
	return indices
}

// withoutSystem returns lines without the system lines of the subtypes.
func withoutSystem(t *testing.T, lines [][]byte, subtypes ...string) [][]byte {
	t.Helper()
	return slices.DeleteFunc(slices.Clone(lines), func(line []byte) bool {
		msg, err := claude.Parse(line)
		if err != nil {
			t.Fatal(err)
		}
		return msg.Type == claude.TypeSystem && slices.Contains(subtypes, msg.Subtype)
	})
}

var finishSignals = []string{
	claude.SubtypeTaskNotification,
	claude.SubtypeTaskUpdated,
	claude.SubtypeBackgroundTasksChanged,
}

func TestGivenBackgroundSubagentRunWhenReadingThenOnlyTheResultAfterTheSubagentFinishesEndsTheTurn(t *testing.T) {
	lines := fixtureLines(t, "testdata/subagent.jsonl")
	results := resultIndices(t, lines)
	if len(results) != 2 {
		t.Fatalf("fixture has results at %v, want two", results)
	}

	got := turnEnds(replay(t, lines))

	if want := results[1:]; !slices.Equal(got, want) {
		t.Errorf("turn ends at lines %v, want %v (results at %v)", got, want, results)
	}
}

func TestGivenStreamWithNoTaskLinesWhenReadingThenEveryResultEndsATurn(t *testing.T) {
	lines := withoutSystem(t, fixtureLines(t, "testdata/subagent.jsonl"),
		append([]string{claude.SubtypeTaskStarted, "task_progress"}, finishSignals...)...)

	got := turnEnds(replay(t, lines))

	if want := resultIndices(t, lines); !slices.Equal(got, want) {
		t.Errorf("turn ends at lines %v, want every result, %v", got, want)
	}
}

func TestGivenBackgroundSubagentWhenOneSignalReportsItFinishedThenTheNextResultEndsTheTurn(t *testing.T) {
	for _, signal := range finishSignals {
		t.Run(signal, func(t *testing.T) {
			var others []string
			for _, other := range finishSignals {
				if other != signal {
					others = append(others, other)
				}
			}
			lines := withoutSystem(t, fixtureLines(t, "testdata/subagent.jsonl"), others...)
			results := resultIndices(t, lines)

			got := turnEnds(replay(t, lines))

			if want := results[1:]; !slices.Equal(got, want) {
				t.Errorf("turn ends at lines %v, want %v", got, want)
			}
		})
	}
}

// Claude Code that reports a subagent started and never finished leaves
// the turn open; the owner interrupts or stops the task.
func TestGivenBackgroundSubagentNeverReportedFinishedWhenReadingThenNoResultEndsTheTurn(t *testing.T) {
	lines := withoutSystem(t, fixtureLines(t, "testdata/subagent.jsonl"), finishSignals...)

	if got := turnEnds(replay(t, lines)); len(got) != 0 {
		t.Errorf("turn ends at lines %v, want none", got)
	}
}

const (
	agentStarted     = `{"type":"system","subtype":"task_started","task_id":"a1","tool_use_id":"toolu_1","task_type":"local_agent","is_backgrounded":true,"session_id":"s"}`
	agentRunning     = `{"type":"system","subtype":"task_updated","task_id":"a1","patch":{"status":"running"},"session_id":"s"}`
	agentNotified    = `{"type":"system","subtype":"task_notification","task_id":"a1","tool_use_id":"toolu_1","status":"completed","summary":"3","session_id":"s"}`
	bashStarted      = `{"type":"system","subtype":"task_started","task_id":"b1","tool_use_id":"toolu_2","task_type":"local_bash","is_backgrounded":true,"session_id":"s"}`
	resultAnswering1 = `{"type":"result","subtype":"success","result":"launched","session_id":"s","user_message_uuid":"prompt-1"}`
	resultPlain      = `{"type":"result","subtype":"success","result":"3","session_id":"s"}`
)

func lines(texts ...string) [][]byte {
	var out [][]byte
	for _, text := range texts {
		out = append(out, []byte(text))
	}
	return out
}

func TestGivenBackgroundBashCommandWhenTheResultArrivesThenItEndsTheTurn(t *testing.T) {
	got := turnEnds(replay(t, lines(bashStarted, resultPlain)))

	if !slices.Equal(got, []int{1}) {
		t.Errorf("turn ends at lines %v, want [1]", got)
	}
}

func TestGivenSubagentUpdatedToANonTerminalStatusWhenTheResultArrivesThenTheTurnGoesOn(t *testing.T) {
	got := turnEnds(replay(t, lines(agentStarted, agentRunning, resultPlain, agentNotified, resultPlain)))

	if !slices.Equal(got, []int{4}) {
		t.Errorf("turn ends at lines %v, want [4]", got)
	}
}

func TestGivenPromptAnsweredBeforeTheSubagentFinishedWhenTheTurnEndsThenTheEndListsThePrompt(t *testing.T) {
	outputs := replay(t, lines(agentStarted, resultAnswering1, agentNotified, resultPlain))

	if first := outputs[1]; first.TurnEnded || !slices.Equal(first.Answering, []string{"prompt-1"}) {
		t.Errorf("first result = %+v", first)
	}
	if last := outputs[3]; !last.TurnEnded || !slices.Equal(last.Answering, []string{"prompt-1"}) || last.SessionID != "s" {
		t.Errorf("last result = %+v", last)
	}
}
