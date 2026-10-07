package claude_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/harness/claude"
)

const fixtureGlob = "../../../spikes/mod-vs-stdout/runs/*/stdout.jsonl"

func fixtureLines(t *testing.T, path string) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var lines [][]byte
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			lines = append(lines, bytes.TrimRight(line, "\n"))
		}
		if err != nil {
			break
		}
	}
	return lines
}

func fixture(t *testing.T, run string) []claude.Message {
	t.Helper()
	var msgs []claude.Message
	for idx, line := range fixtureLines(t, filepath.Join("../../../spikes/mod-vs-stdout/runs", run, "stdout.jsonl")) {
		msg, err := claude.Parse(line)
		if err != nil {
			t.Fatalf("%s line %d: %v", run, idx+1, err)
		}
		msgs = append(msgs, msg)
	}
	return msgs
}

// assertSameJSON checks that msg marshals to JSON equal to line. Bytes may
// differ: encoding/json escapes <, > and & in MarshalJSON output.
func assertSameJSON(t *testing.T, msg claude.Message, line []byte) {
	t.Helper()
	encoded, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got, want any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("unmarshal marshalled message: %v", err)
	}
	if err := json.Unmarshal(line, &want); err != nil {
		t.Fatalf("unmarshal line: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("marshalled %s, want %s", encoded, line)
	}
}

func results(msgs []claude.Message) []claude.Message {
	var out []claude.Message
	for _, msg := range msgs {
		if _, ok := msg.Result(); ok {
			out = append(out, msg)
		}
	}
	return out
}

func TestGivenEverySpikeFixtureWhenParsingEachLineThenNoneFailsAndTheLineIsKeptVerbatim(t *testing.T) {
	paths, err := filepath.Glob(fixtureGlob)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 11 {
		t.Fatalf("found %d fixtures, want 11", len(paths))
	}
	for _, path := range paths {
		lines := fixtureLines(t, path)
		if len(lines) == 0 {
			t.Errorf("%s: no lines", path)
		}
		for idx, line := range lines {
			msg, err := claude.Parse(line)
			if err != nil {
				t.Errorf("%s line %d: %v", path, idx+1, err)
				continue
			}
			if msg.Type == "" {
				t.Errorf("%s line %d: empty type", path, idx+1)
			}
			if !bytes.Equal(msg.Raw, line) {
				t.Errorf("%s line %d: Raw differs from the line", path, idx+1)
			}
			assertSameJSON(t, msg, line)
		}
	}
}

func TestGivenUnknownMessageTypeWhenParsingThenTypeAndFieldsArePreserved(t *testing.T) {
	line := []byte(`{"type":"future_message","subtype":"later","session_id":"s-1","payload":{"nested":[1,2]}}`)

	msg, err := claude.Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if msg.Type != "future_message" || msg.Subtype != "later" || msg.SessionID != "s-1" {
		t.Errorf("envelope = %q/%q/%q", msg.Type, msg.Subtype, msg.SessionID)
	}
	assertSameJSON(t, msg, line)
}

func TestGivenKnownMessageTypeWithUnknownFieldWhenParsingThenFieldIsPreserved(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","session_id":"s-1","num_turns":1,"brand_new":{"x":true}}`)

	msg, err := claude.Parse(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := msg.Result(); !ok {
		t.Fatal("not decoded as a result")
	}
	assertSameJSON(t, msg, line)
}

func TestGivenUndocumentedMessageTypesInFixturesWhenParsingThenTheyAreKeptUnmodelled(t *testing.T) {
	var seen []string
	for _, msg := range fixture(t, "control-1") {
		if msg.Type == "command_lifecycle" {
			seen = append(seen, msg.Type)
			if _, ok := msg.Result(); ok {
				t.Error("command_lifecycle decoded as a result")
			}
		}
	}
	if len(seen) == 0 {
		t.Fatal("control-1 has no command_lifecycle message")
	}
}

func TestGivenPauseStdinRunWhenReadingResultsThenEachEndsATurnWithItsFigures(t *testing.T) {
	got := results(fixture(t, "pause-stdin"))

	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (the paused turn and the resume turn)", len(got))
	}
	first := got[0]
	res, _ := first.Result()
	if first.SessionID != "9cbf22cd-b8fe-473d-ad85-cf8dcdae5a9a" {
		t.Errorf("session_id = %q", first.SessionID)
	}
	if first.Subtype != "success" || res.IsError {
		t.Errorf("subtype = %q, is_error = %v", first.Subtype, res.IsError)
	}
	wantUsage := claude.Usage{InputTokens: 18, OutputTokens: 316, CacheCreationInputTokens: 7816, CacheReadInputTokens: 35544}
	if res.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", res.Usage, wantUsage)
	}
	if res.TotalCostUSD != 0.0207844 {
		t.Errorf("total_cost_usd = %v", res.TotalCostUSD)
	}
	if res.NumTurns != 2 {
		t.Errorf("num_turns = %d", res.NumTurns)
	}
	if res.TerminalReason != "completed" {
		t.Errorf("terminal_reason = %q", res.TerminalReason)
	}
	if !strings.HasPrefix(res.Result, "DONE-1") {
		t.Errorf("result = %q", res.Result)
	}

	second, _ := got[1].Result()
	if second.Result != "DONE-3\n\nFINISHED" {
		t.Errorf("resume result = %q", second.Result)
	}
	if second.TotalCostUSD <= res.TotalCostUSD {
		t.Errorf("total_cost_usd %v did not grow from %v; it is cumulative per process", second.TotalCostUSD, res.TotalCostUSD)
	}
}

func TestGivenInterruptedTurnWhenReadingItsResultThenItIsAnErrorSubtype(t *testing.T) {
	got := results(fixture(t, "pause-interrupt"))

	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	res, _ := got[0].Result()
	if got[0].Subtype != "error_during_execution" || !res.IsError {
		t.Errorf("subtype = %q, is_error = %v", got[0].Subtype, res.IsError)
	}
	if res.TerminalReason != "aborted_tools" {
		t.Errorf("terminal_reason = %q", res.TerminalReason)
	}
	if res.Result != "" || len(res.Errors) == 0 {
		t.Errorf("result = %q, errors = %q; want no text and some errors", res.Result, res.Errors)
	}
	if res.NumTurns != 3 {
		t.Errorf("num_turns = %d", res.NumTurns)
	}
}

func TestGivenEveryFixtureWhenReadingInitThenItNamesTheSessionOfItsResultsAndTheModel(t *testing.T) {
	paths, _ := filepath.Glob(fixtureGlob)
	for _, path := range paths {
		run := filepath.Base(filepath.Dir(path))
		msgs := fixture(t, run)
		sessions := map[string]bool{}
		inits := 0
		for _, msg := range msgs {
			init, ok := msg.Init()
			if !ok {
				continue
			}
			inits++
			sessions[msg.SessionID] = true
			if init.Model != "claude-haiku-4-5-20251001" {
				t.Errorf("%s: model = %q", run, init.Model)
			}
			if init.ClaudeCodeVersion != "2.1.289" {
				t.Errorf("%s: claude_code_version = %q", run, init.ClaudeCodeVersion)
			}
		}
		if inits == 0 {
			t.Errorf("%s: no system/init", run)
		}
		if len(sessions) != 1 {
			t.Errorf("%s: init carries %d session ids, want 1", run, len(sessions))
		}
		for _, msg := range results(msgs) {
			if !sessions[msg.SessionID] {
				t.Errorf("%s: result session %q not announced by init", run, msg.SessionID)
			}
		}
	}
}

func TestGivenPauseStdinRunWhenReadingInitThenItDescribesTheSession(t *testing.T) {
	msgs := fixture(t, "pause-stdin")

	init, ok := msgs[0].Init()
	if !ok {
		t.Fatalf("first line is %s/%s, want system/init", msgs[0].Type, msgs[0].Subtype)
	}
	if msgs[0].SessionID != "9cbf22cd-b8fe-473d-ad85-cf8dcdae5a9a" {
		t.Errorf("session_id = %q", msgs[0].SessionID)
	}
	if init.PermissionMode != "default" || init.Cwd != "/private/tmp/modspike-pause-stdin-afj08wc_" {
		t.Errorf("permissionMode = %q, cwd = %q", init.PermissionMode, init.Cwd)
	}
	if !slices.Contains(init.Capabilities, "interrupt_receipt_v1") {
		t.Errorf("capabilities = %q", init.Capabilities)
	}
}

func TestGivenPauseStdinRunWhenReadingRateLimitEventThenItCarriesTheObservedQuotaFields(t *testing.T) {
	var events []claude.RateLimitInfo
	for _, msg := range fixture(t, "pause-stdin") {
		if info, ok := msg.RateLimit(); ok {
			events = append(events, info)
		}
	}

	if len(events) != 1 {
		t.Fatalf("got %d rate_limit_events, want 1", len(events))
	}
	got := events[0]
	if got.Status != "allowed" || got.ResetsAt != 1791375000 || got.RateLimitType != "five_hour" {
		t.Errorf("status = %q, resetsAt = %d, rateLimitType = %q", got.Status, got.ResetsAt, got.RateLimitType)
	}
	if got.OverageStatus != "rejected" || got.OverageDisabledReason != "out_of_credits" || got.IsUsingOverage {
		t.Errorf("overage = %q/%q/%v", got.OverageStatus, got.OverageDisabledReason, got.IsUsingOverage)
	}
	if got.Utilization != nil {
		t.Errorf("utilization = %v, want absent at the top level", *got.Utilization)
	}
	want := map[string]claude.RateLimitWindow{
		"five_hour": {Utilization: 0.29, ResetsAt: 1791375000},
		"seven_day": {Utilization: 0.07, ResetsAt: 1791651600},
	}
	if len(got.UnifiedWindows) != len(want) {
		t.Fatalf("unifiedWindows = %+v", got.UnifiedWindows)
	}
	for name, window := range want {
		if got.UnifiedWindows[name] != window {
			t.Errorf("unifiedWindows[%s] = %+v, want %+v", name, got.UnifiedWindows[name], window)
		}
	}
}

// The pause-submit run's extra turn came from the mod's $.prompt.submit,
// which gave it a command uuid; Claude Code echoed that uuid as
// user_message_uuid. No stdin message in the spike carried a uuid.
func TestGivenPauseSubmitRunWhenReadingEchoesThenTheTurnsFirstReplyAndResultCarryTheUUID(t *testing.T) {
	const uuid = "aaec451a-9eee-4e9a-9199-22f956262de6"
	var echoed []string
	for _, msg := range fixture(t, "pause-submit") {
		if slices.Contains(msg.Answering(), uuid) {
			echoed = append(echoed, msg.Type+"/"+msg.Subtype)
		}
	}

	want := []string{"system/thinking_tokens", "system/thinking_tokens", "assistant/", "result/success"}
	if !slices.Equal(echoed, want) {
		t.Errorf("echoed on %q, want %q", echoed, want)
	}
}

func TestGivenPauseStdinRunWhenReadingEchoesThenNoneArePresentBecauseNoUUIDWasSent(t *testing.T) {
	for idx, msg := range fixture(t, "pause-stdin") {
		if msg.Answering() != nil {
			t.Errorf("line %d echoes %q", idx+1, msg.Answering())
		}
	}
}

func TestGivenMalformedLineWhenParsingThenErrMalformed(t *testing.T) {
	lines := map[string]string{
		"not JSON":                     `not json`,
		"array":                        `[1,2]`,
		"no type":                      `{"session_id":"s"}`,
		"non-string type":              `{"type":7}`,
		"result with string num_turns": `{"type":"result","num_turns":"two"}`,
		"rate limit without info":      `{"type":"rate_limit_event"}`,
		"init with non-string model":   `{"type":"system","subtype":"init","model":1}`,
	}
	for name, line := range lines {
		t.Run(name, func(t *testing.T) {
			_, err := claude.Parse([]byte(line))
			if !errors.Is(err, claude.ErrMalformed) {
				t.Errorf("err = %v, want ErrMalformed", err)
			}
		})
	}
}
