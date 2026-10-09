package protocol_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenStartTaskCommandWhenMarshallingThenItHasTheWireFieldNames(t *testing.T) {
	payload, err := json.Marshal(protocol.StartTask{
		Prompt:       "fix the bug",
		SystemPrompt: "be brief",
		Workspace:    &protocol.Workspace{Repo: "git@example.com:o/r.git", Ref: "main"},
		Model:        "haiku",
		PauseLimits:  protocol.PauseLimits{Acknowledge: 2 * time.Minute, Cleanup: 90 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	command := protocol.Command{
		ID:       3,
		DaemonID: "laptop",
		TaskID:   "task-1",
		Kind:     protocol.CommandStartTask,
		Time:     time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
		Payload:  payload,
	}

	got, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}

	want := `{"id":3,"daemon_id":"laptop","task_id":"task-1","kind":"start_task","time":"2026-10-07T12:00:00Z","payload":{"prompt":"fix the bug","system_prompt":"be brief","workspace":{"repo":"git@example.com:o/r.git","ref":"main"},"model":"haiku","pause_limits":{"acknowledge":"2m0s","cleanup":"1m30s"}}}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenCommandWithoutPayloadWhenMarshallingThenPayloadIsOmitted(t *testing.T) {
	command := protocol.Command{ID: 4, DaemonID: "laptop", TaskID: "task-1", Kind: protocol.CommandPause, Time: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)}

	got, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}

	want := `{"id":4,"daemon_id":"laptop","task_id":"task-1","kind":"pause","time":"2026-10-07T12:00:00Z"}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenDurationStringsWhenUnmarshallingPauseLimitsThenTheyAreParsed(t *testing.T) {
	var got protocol.PauseLimits
	if err := json.Unmarshal([]byte(`{"acknowledge":"30s","cleanup":"5m"}`), &got); err != nil {
		t.Fatal(err)
	}

	want := protocol.PauseLimits{Acknowledge: 30 * time.Second, Cleanup: 5 * time.Minute}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestGivenMissingOrMalformedDurationWhenUnmarshallingPauseLimitsThenItFails(t *testing.T) {
	for _, raw := range []string{`{"acknowledge":"30s"}`, `{"acknowledge":"30","cleanup":"5m"}`, `{"acknowledge":30,"cleanup":"5m"}`} {
		var got protocol.PauseLimits
		if err := json.Unmarshal([]byte(raw), &got); err == nil {
			t.Errorf("Unmarshal(%s) = %+v, want an error", raw, got)
		}
	}
}

func TestGivenAnswerPermissionWhenMarshallingThenItHasTheWireFieldNames(t *testing.T) {
	got, err := json.Marshal(protocol.AnswerPermission{RequestID: "req-1", Allow: false, Message: "not that file"})
	if err != nil {
		t.Fatal(err)
	}

	want := `{"request_id":"req-1","allow":false,"message":"not that file"}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenSafeNameWhenParsingDaemonIDThenItIsAccepted(t *testing.T) {
	for _, raw := range []string{"d", "laptop", "vps-1.eu_west", "A1"} {
		got, err := protocol.ParseDaemonID(raw)
		if err != nil || string(got) != raw {
			t.Errorf("ParseDaemonID(%q) = %q, %v", raw, got, err)
		}
	}
}

func TestGivenUnsafeNameWhenParsingDaemonIDThenErrInvalidDaemonID(t *testing.T) {
	long := make([]byte, 129)
	for idx := range long {
		long[idx] = 'a'
	}
	for _, raw := range []string{"", ".", "..", "a/b", "../x", "a b", "a?b", "a\x00", string(long)} {
		if _, err := protocol.ParseDaemonID(raw); !errors.Is(err, protocol.ErrInvalidDaemonID) {
			t.Errorf("ParseDaemonID(%q) err = %v, want ErrInvalidDaemonID", raw, err)
		}
	}
}

func TestGivenPromptWhenMarshallingThenFromAppearsOnlyWhenSet(t *testing.T) {
	child := protocol.TaskID("child-1")
	for want, prompt := range map[string]protocol.Prompt{
		`{"text":"more"}`: {Text: "more"},
		`{"text":"Message from task child-1: PEAR","from":"child-1"}`: {Text: "Message from task child-1: PEAR", From: &child},
	} {
		got, err := json.Marshal(prompt)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
	}
}

func TestGivenStartTaskToolsWhenMarshallingThenNilIsOmittedAndEmptyIsKept(t *testing.T) {
	for want, start := range map[string]protocol.StartTask{
		`{"prompt":"p","pause_limits":{"acknowledge":"0s","cleanup":"0s"}}`:                          {Prompt: "p"},
		`{"prompt":"p","pause_limits":{"acknowledge":"0s","cleanup":"0s"},"tools":[]}`:               {Prompt: "p", Tools: []string{}},
		`{"prompt":"p","pause_limits":{"acknowledge":"0s","cleanup":"0s"},"tools":["send_message"]}`: {Prompt: "p", Tools: []string{protocol.ToolSendMessage}},
	} {
		got, err := json.Marshal(start)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("got  %s\nwant %s", got, want)
		}
		var back protocol.StartTask
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatal(err)
		}
		if (back.Tools == nil) != (start.Tools == nil) || len(back.Tools) != len(start.Tools) {
			t.Errorf("%s decodes to tools %#v, want %#v", got, back.Tools, start.Tools)
		}
	}
}
