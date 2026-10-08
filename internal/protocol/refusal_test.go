package protocol_test

import (
	"encoding/json"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenEventsRefusedWhenMarshallingThenItHasTheWireFieldNames(t *testing.T) {
	got, err := json.Marshal(protocol.EventsRefused{Reason: protocol.RefusedTaskMoved, TaskID: "task-1", Message: "moved"})
	if err != nil {
		t.Fatal(err)
	}

	want := `{"reason":"task_moved","task_id":"task-1","message":"moved"}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
