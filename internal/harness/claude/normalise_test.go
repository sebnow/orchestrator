package claude_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// normaliseRun normalises every line of a spike run, in order.
func normaliseRun(t *testing.T, run string) []transcript.Body {
	t.Helper()
	var bodies []transcript.Body
	for _, line := range fixtureLines(t, filepath.Join("../../../spikes/mod-vs-stdout/runs", run, "stdout.jsonl")) {
		bodies = append(bodies, claude.Normalise(line)...)
	}
	return bodies
}

func bodiesOf[T transcript.Body](bodies []transcript.Body) []T {
	var out []T
	for _, body := range bodies {
		if v, ok := body.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func TestGivenEverySpikeFixtureWhenNormalisingThenEveryLineIsAccountedForAndEveryToolResultAnswersAnEarlierCall(t *testing.T) {
	paths, err := filepath.Glob(fixtureGlob)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 11 {
		t.Fatalf("found %d fixtures, want 11", len(paths))
	}
	for _, path := range paths {
		calls := map[string]bool{}
		for idx, line := range fixtureLines(t, path) {
			msg, err := claude.Parse(line)
			if err != nil {
				t.Fatalf("%s line %d: %v", path, idx+1, err)
			}
			bodies := claude.Normalise(line)
			_, isInit := msg.Init()
			_, isRateLimit := msg.RateLimit()
			if (len(bodies) == 0) != (isInit || isRateLimit) {
				t.Errorf("%s line %d (%s/%s): %d bodies", path, idx+1, msg.Type, msg.Subtype, len(bodies))
			}
			for _, body := range bodies {
				switch body := body.(type) {
				case nil:
					t.Errorf("%s line %d: nil body", path, idx+1)
				case transcript.ToolCall:
					calls[body.ID] = true
				case transcript.ToolResult:
					if !calls[body.ToolCallID] {
						t.Errorf("%s line %d: result for %q, which no earlier tool call has", path, idx+1, body.ToolCallID)
					}
				case transcript.Unknown:
					if body.Type == "assistant" || body.Type == "result" || strings.HasPrefix(body.Type, "assistant/") {
						t.Errorf("%s line %d: %s passed on as unknown", path, idx+1, body.Type)
					}
				}
			}
		}
	}
}

func TestGivenMCPRunWhenNormalisingThenTheDeniedCallIsPairedWithAnErrorResultCarryingTheDenial(t *testing.T) {
	bodies := normaliseRun(t, "mcp")

	var call *transcript.ToolCall
	var result *transcript.ToolResult
	for idx, body := range bodies {
		if c, ok := body.(transcript.ToolCall); ok && c.ID == "toolu_016DTgZunDRfvZtABatFZCQD" {
			call = &c
			for _, later := range bodies[idx+1:] {
				if r, ok := later.(transcript.ToolResult); ok && r.ToolCallID == c.ID {
					result = &r
					break
				}
			}
		}
	}
	if call == nil || result == nil {
		t.Fatalf("call = %+v, result = %+v; want both", call, result)
	}
	var input map[string]string
	if err := json.Unmarshal(call.Input, &input); err != nil {
		t.Fatal(err)
	}
	if call.Name != "Bash" || input["command"] != "touch spike-denied.txt" {
		t.Errorf("call = %s %s", call.Name, call.Input)
	}
	want := transcript.ToolResult{ToolCallID: call.ID, Content: "Denied by the spike receiver acting as the daemon.", IsError: true}
	if *result != want {
		t.Errorf("result = %+v, want %+v", *result, want)
	}
}

func TestGivenMCPRunWhenNormalisingThenEachTurnEndsWithItsUsageAndRunningCost(t *testing.T) {
	got := bodiesOf[transcript.TurnEnded](normaliseRun(t, "mcp"))

	want := []transcript.TurnEnded{
		{Outcome: "success", StopReason: "end_turn", NumTurns: 3, Duration: 4084 * time.Millisecond, TotalCostUSD: 0.0204534,
			InputTokens: 18, OutputTokens: 329, CacheCreationInputTokens: 7518, CacheReadInputTokens: 37544, ContextWindow: 200000},
		{IsError: true, Outcome: "error_during_execution", StopReason: "tool_use", NumTurns: 3, Duration: 4618 * time.Millisecond, TotalCostUSD: 0.0239514,
			InputTokens: 10, OutputTokens: 172, CacheCreationInputTokens: 179, CacheReadInputTokens: 22700, ContextWindow: 200000},
		{Outcome: "success", StopReason: "end_turn", NumTurns: 1, Duration: 844 * time.Millisecond, TotalCostUSD: 0.027040300000000003,
			InputTokens: 10, OutputTokens: 35, CacheCreationInputTokens: 308, CacheReadInputTokens: 22879, ContextWindow: 200000},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestGivenPauseInterruptRunWhenNormalisingThenTheInterruptedTurnEndsInErrorAfterTheRejectedCall(t *testing.T) {
	var bodies []transcript.Body
	for _, body := range normaliseRun(t, "pause-interrupt") {
		if u, ok := body.(transcript.Unknown); ok && u.Type == "system/thinking_tokens" {
			continue
		}
		bodies = append(bodies, body)
	}

	var kinds []transcript.Kind
	for _, body := range bodies[:8] {
		kinds = append(kinds, body.Kind())
	}
	wantKinds := []transcript.Kind{
		transcript.KindAgentThinking, transcript.KindToolCall, transcript.KindUnknown, transcript.KindToolResult,
		transcript.KindUnknown, transcript.KindTurnEnded, transcript.KindAgentThinking, transcript.KindToolCall,
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("kinds = %v\nwant    %v", kinds, wantKinds)
	}
	if u := bodies[2].(transcript.Unknown); u.Type != "control_response" {
		t.Errorf("unknown = %+v, want the control_response", u)
	}
	if r := bodies[3].(transcript.ToolResult); r.ToolCallID != "toolu_01APiAvLGzuFQJhTz5Hw2aPj" || !r.IsError {
		t.Errorf("result = %+v", r)
	}
	if u := bodies[4].(transcript.Unknown); u.Type != "user/text" || !strings.Contains(string(u.Raw), "[Request interrupted by user for tool use]") {
		t.Errorf("unknown = %+v, want the interrupt notice", u)
	}
	want := transcript.TurnEnded{IsError: true, Outcome: "error_during_execution", StopReason: "tool_use", NumTurns: 3,
		Duration: 6780 * time.Millisecond, TotalCostUSD: 0.0167917,
		InputTokens: 10, OutputTokens: 189, CacheCreationInputTokens: 7210, CacheReadInputTokens: 14167, ContextWindow: 200000}
	if got := bodies[5].(transcript.TurnEnded); got != want {
		t.Errorf("turn = %+v\nwant   %+v", got, want)
	}
	texts := bodiesOf[transcript.AgentText](bodies)
	if len(texts) != 3 || texts[2].Text != "DONE-3\n\nFINISHED" {
		t.Errorf("texts = %+v", texts)
	}
}

func TestGivenLinesTheFixturesLackWhenNormalisingThenEachHasItsBody(t *testing.T) {
	for name, tc := range map[string]struct {
		line string
		want []transcript.Body
	}{
		"thinking with text": {
			`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"plan","signature":"x"},{"type":"redacted_thinking","data":"y"}]}}`,
			[]transcript.Body{transcript.AgentThinking{Text: "plan"}, transcript.AgentThinking{}},
		},
		"tool result as blocks": {
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]}]}}`,
			[]transcript.Body{transcript.ToolResult{ToolCallID: "t1", Content: "a\n[image]\nb"}},
		},
		"unmodelled assistant block": {
			`{"type":"assistant","message":{"content":[{"type":"server_tool_use","id":"s1"}]}}`,
			[]transcript.Body{transcript.Unknown{RecordKind: "harness_output", Type: "assistant/server_tool_use", Raw: json.RawMessage(`{"type":"server_tool_use","id":"s1"}`)}},
		},
		"user prompt as a string": {
			`{"type":"user","message":{"content":"hello"}}`,
			[]transcript.Body{transcript.Unknown{RecordKind: "harness_output", Type: "user", Raw: json.RawMessage(`{"type":"user","message":{"content":"hello"}}`)}},
		},
		"max turns result": {
			`{"type":"result","subtype":"error_max_turns","is_error":true,"result":null,"num_turns":9,"duration_ms":5,"total_cost_usd":0.5}`,
			[]transcript.Body{transcript.TurnEnded{IsError: true, Outcome: "error_max_turns", NumTurns: 9, Duration: 5 * time.Millisecond, TotalCostUSD: 0.5}},
		},
		"malformed result": {
			`{"type":"result","subtype":"success","num_turns":"many"}`,
			[]transcript.Body{transcript.Unknown{RecordKind: "harness_output", Type: "result/success", Raw: json.RawMessage(`{"type":"result","subtype":"success","num_turns":"many"}`)}},
		},
		"line that is not JSON": {
			`"Error: not logged in"`,
			[]transcript.Body{transcript.Unknown{RecordKind: "harness_output", Raw: json.RawMessage(`"Error: not logged in"`)}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := claude.Normalise(json.RawMessage(tc.line))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

// The subagent fixture follows a recorded run; see testdata/README.md.
// The subagent runs in the background: the turn that started it ends
// before the subagent's messages arrive, and the harness starts a turn of
// its own to report the subagent's result.
func TestGivenSubagentRunWhenNormalisingThenTheSubagentsBodiesCarryTheSpawningCallAndTheMainConversationsDoNot(t *testing.T) {
	var bodies []transcript.Body
	for _, line := range fixtureLines(t, "testdata/subagent.jsonl") {
		bodies = append(bodies, claude.Normalise(line)...)
	}

	type entry struct {
		kind   transcript.Kind
		parent string
	}
	var got []entry
	for _, body := range bodies {
		got = append(got, entry{body.Kind(), transcript.ParentToolUseID(body)})
	}
	const agent = "toolu_fixture_agent"
	want := []entry{
		{transcript.KindAgentThinking, ""},
		{transcript.KindToolCall, ""},
		{transcript.KindUnknown, ""},
		{transcript.KindUnknown, ""},
		{transcript.KindToolResult, ""},
		{transcript.KindAgentText, ""},
		{transcript.KindTurnEnded, ""},
		{transcript.KindAgentThinking, agent},
		{transcript.KindToolCall, agent},
		{transcript.KindUnknown, ""},
		{transcript.KindToolResult, agent},
		{transcript.KindAgentText, agent},
		{transcript.KindUnknown, ""},
		{transcript.KindUnknown, ""},
		{transcript.KindUnknown, ""},
		{transcript.KindAgentText, ""},
		{transcript.KindTurnEnded, ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bodies = %+v\nwant     %+v", got, want)
	}
	texts := bodiesOf[transcript.AgentText](bodies)
	if len(texts) != 3 || texts[0].ParentToolUseID != "" ||
		texts[1] != (transcript.AgentText{Text: "3", ParentToolUseID: agent}) || texts[2] != (transcript.AgentText{Text: "3"}) {
		t.Errorf("texts = %+v", texts)
	}
}
