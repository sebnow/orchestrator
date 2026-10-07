package protocol_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenEventWhenMarshallingThenEnvelopeHasTheWireFieldNames(t *testing.T) {
	event := protocol.Event{
		TaskID:  "task-1",
		Seq:     7,
		Kind:    protocol.KindPauseAcknowledged,
		Harness: protocol.Harness{Name: "claude-code", Version: "2.1.289"},
		Time:    time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
		Payload: json.RawMessage(`{"note":"stopped after step 1"}`),
	}

	got, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}

	want := `{"task_id":"task-1","seq":7,"kind":"pause_acknowledged","harness":{"name":"claude-code","version":"2.1.289"},"time":"2026-10-07T12:00:00Z","payload":{"note":"stopped after step 1"}}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestGivenSafeNameWhenParsingTaskIDThenItIsAccepted(t *testing.T) {
	for _, raw := range []string{"t", "task-1", "2026.10.07_a-b", "A1"} {
		got, err := protocol.ParseTaskID(raw)
		if err != nil || string(got) != raw {
			t.Errorf("ParseTaskID(%q) = %q, %v", raw, got, err)
		}
	}
}

func TestGivenUnsafeNameWhenParsingTaskIDThenErrInvalidTaskID(t *testing.T) {
	long := make([]byte, 129)
	for idx := range long {
		long[idx] = 'a'
	}
	for _, raw := range []string{"", ".", "..", "a/b", "../x", "a b", "tâche", "a\x00", string(long)} {
		if _, err := protocol.ParseTaskID(raw); !errors.Is(err, protocol.ErrInvalidTaskID) {
			t.Errorf("ParseTaskID(%q) err = %v, want ErrInvalidTaskID", raw, err)
		}
	}
}
