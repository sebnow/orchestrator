package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

var historyStart = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func at(seconds float64) time.Time {
	return historyStart.Add(time.Duration(seconds * float64(time.Second)))
}

func historyEvent(seq uint64, seconds float64, kind protocol.Kind, harnessName, payload string) protocol.Event {
	return protocol.Event{
		TaskID: "task-1", Seq: seq, Kind: kind, Harness: protocol.Harness{Name: harnessName, Version: "1"},
		Time: at(seconds), Payload: json.RawMessage(payload),
	}
}

func historyCommand(id uint64, seconds float64, kind protocol.CommandKind, payload string) protocol.Command {
	command := protocol.Command{ID: id, DaemonID: "laptop", TaskID: "task-1", Kind: kind, Time: at(seconds)}
	if payload != "" {
		command.Payload = json.RawMessage(payload)
	}
	return command
}

func TestGivenMixedHistoryWhenAssemblingThenEachListKeepsItsOrderAndTheyInterleaveByTime(t *testing.T) {
	events := []protocol.Event{
		historyEvent(1, 1, protocol.KindHarnessStarted, claude.Name, `{"pid":7,"model":"haiku","workdir":"/w"}`),
		historyEvent(2, 3, protocol.KindHarnessOutput, claude.Name,
			`{"type":"assistant","message":{"content":[{"type":"text","text":"on it"},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}}`),
		historyEvent(3, 4, protocol.KindPermissionRequested, claude.Name, `{"request_id":"r1","tool":"Bash","input":{"command":"ls"}}`),
		historyEvent(4, 6, protocol.KindPauseSettled, claude.Name, `{"interrupted":true}`),
		historyEvent(5, 7, protocol.KindHarnessOutput, "other-harness", `{"type":"assistant"}`),
		historyEvent(6, 8, "mystery", claude.Name, `{"x":1}`),
		historyEvent(7, 9, protocol.KindHarnessExited, claude.Name, `{"exit_code":0}`),
	}
	commands := []protocol.Command{
		historyCommand(10, 0, protocol.CommandStartTask, `{"prompt":"list files","pause_limits":{"acknowledge":"1m0s","cleanup":"5m0s"}}`),
		historyCommand(11, 5, protocol.CommandAnswerPermission, `{"request_id":"r1","allow":true}`),
		// Issued after command 11 but stamped earlier: id order wins.
		historyCommand(12, 4.5, protocol.CommandPause, ""),
		historyCommand(13, 9, protocol.CommandResume, ""),
		historyCommand(14, 10, "reboot", `{"now":true}`),
	}

	got := assemble("task-1", history{events: events, commands: commands})

	type step struct {
		Source transcript.Source
		Time   time.Time
		Body   transcript.Body
	}
	fromEvent := func(seq uint64) transcript.Source { return transcript.Source{TaskID: "task-1", Seq: seq} }
	fromCommand := func(id uint64) transcript.Source { return transcript.Source{TaskID: "task-1", CommandID: id} }
	want := []step{
		{fromCommand(10), at(0), transcript.OwnerPrompt{Text: "list files"}},
		{fromEvent(1), at(1), transcript.HarnessStarted{PID: 7, Model: "haiku", Workdir: "/w"}},
		{fromEvent(2), at(3), transcript.AgentText{Text: "on it"}},
		{fromEvent(2), at(3), transcript.ToolCall{ID: "t1", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}},
		{fromEvent(3), at(4), transcript.PermissionRequested{RequestID: "r1", Tool: "Bash", Input: json.RawMessage(`{"command":"ls"}`)}},
		{fromCommand(11), at(5), transcript.PermissionAnswered{RequestID: "r1", Allow: true}},
		{fromCommand(12), at(4.5), transcript.PauseRequested{}},
		{fromEvent(4), at(6), transcript.PauseSettled{Interrupted: true}},
		{fromEvent(5), at(7), transcript.Unknown{RecordKind: "harness_output", Raw: json.RawMessage(`{"type":"assistant"}`)}},
		{fromEvent(6), at(8), transcript.Unknown{RecordKind: "mystery", Raw: json.RawMessage(`{"x":1}`)}},
		{fromEvent(7), at(9), transcript.HarnessExited{}},
		{fromCommand(13), at(9), transcript.OwnerPrompt{Resume: true}},
		{fromCommand(14), at(10), transcript.Unknown{RecordKind: "reboot", Raw: json.RawMessage(`{"now":true}`)}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for idx := range want {
		g := step{got[idx].Source, got[idx].Time, got[idx].Body}
		if !reflect.DeepEqual(g, want[idx]) {
			t.Errorf("entry %d = %+v\nwant      %+v", idx, g, want[idx])
		}
	}
}

func TestGivenUndecodableControlEventWhenAssemblingThenItIsKeptAsUnknown(t *testing.T) {
	got := assemble("task-1", history{events: []protocol.Event{historyEvent(1, 0, protocol.KindHarnessExited, claude.Name, `"oops"`)}})

	want := transcript.Unknown{RecordKind: "harness_exited", Raw: json.RawMessage(`"oops"`)}
	if len(got) != 1 || !reflect.DeepEqual(got[0].Body, want) {
		t.Errorf("got %+v, want one %+v", got, want)
	}
}

func TestGivenStoredTaskWhenReadingItsTranscriptThenItsEventsAndCommandsAreBothThere(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	exited := event("task-1", 1, `{"exit_code":0}`)
	exited.Kind = protocol.KindHarnessExited
	exited.Time = time.Now().Add(time.Hour)
	if _, _, err := srv.store.appendEvents(t.Context(), "laptop", []protocol.Event{exited}); err != nil {
		t.Fatal(err)
	}

	entries, err := srv.Transcript(t.Context(), "task-1")
	if err != nil {
		t.Fatal(err)
	}

	var kinds []transcript.Kind
	for _, entry := range entries {
		kinds = append(kinds, entry.Body.Kind())
	}
	if want := []transcript.Kind{transcript.KindOwnerPrompt, transcript.KindHarnessExited}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
	if _, err := srv.Transcript(t.Context(), "ghost"); !errors.Is(err, errUnknownTask) {
		t.Errorf("transcript of an unknown task: err = %v, want errUnknownTask", err)
	}
}

// signalled reports whether changed holds a signal, without waiting:
// the server signals before it responds, so a signal owed is already there.
func signalled(changed <-chan struct{}) bool {
	select {
	case <-changed:
		return true
	default:
		return false
	}
}

func TestGivenWatchedTaskWhenEventsAreStoredOrCommandsIssuedThenOnlyItsWatchersAreSignalled(t *testing.T) {
	srv := startTestServer(t)
	seedTask(t, srv.store, "laptop", "task-1")
	seedTask(t, srv.store, "laptop", "task-2")
	changed, stop := srv.WatchTask("task-1")
	other, stopOther := srv.WatchTask("task-2")
	defer stopOther()

	postEvents(t, srv, "laptop", event("task-1", 1, `{}`))
	if !signalled(changed) {
		t.Error("no signal after an event was stored")
	}
	postForCommand(t, srv.url+"/v1/tasks/task-1/commands", `{"kind":"pause"}`)
	if !signalled(changed) {
		t.Error("no signal after a command was issued")
	}
	if signalled(other) {
		t.Error("the other task's watcher was signalled")
	}

	stop()
	postForCommand(t, srv.url+"/v1/tasks/task-1/commands", `{"kind":"resume"}`)
	if signalled(changed) {
		t.Error("signalled after the watch stopped")
	}
	if status, body := doRequest(t, http.MethodPost, srv.url+"/v1/tasks/task-2/commands", `{"kind":"stop"}`); status != http.StatusCreated || !signalled(other) {
		t.Errorf("status %d (%s); want the other task's watcher signalled", status, body)
	}
}

func TestGivenMessagesAndChildrenWhenAssemblingThenTheyInterleaveByTimeWithTheTasksOwnRecords(t *testing.T) {
	parent, child, other := protocol.TaskID("task-1"), protocol.TaskID("child"), protocol.TaskID("other")
	childStart := historyCommand(11, 2, protocol.CommandStartTask, `{"prompt":"Say PEAR.","pause_limits":{"acknowledge":"1m0s","cleanup":"5m0s"}}`)
	childStart.TaskID = child
	notice := "Your child task other has ended as failed. It will send no more messages."
	h := history{
		events: []protocol.Event{historyEvent(1, 1, protocol.KindHarnessStarted, claude.Name, `{"pid":7}`)},
		commands: []protocol.Command{
			historyCommand(10, 0, protocol.CommandStartTask, `{"prompt":"Plan.","pause_limits":{"acknowledge":"1m0s","cleanup":"5m0s"}}`),
			childStart,
			historyCommand(13, 5, protocol.CommandPrompt, `{"text":"(as worded)","from":"child"}`),
			historyCommand(14, 6, protocol.CommandPrompt, `{"text":"Message from task child: lost","from":"child"}`),
			historyCommand(15, 7, protocol.CommandPrompt, `{"text":"Carry on."}`),
		},
		messages: []storedMessage{
			{ID: 1, From: &parent, To: child, Text: "Go.", CreatedAt: at(3)},
			{ID: 2, From: &child, To: parent, Text: "PEAR", CreatedAt: at(3.5), DeliveredBy: 13},
			{ID: 3, About: &other, AboutState: TaskFailed, To: parent, Text: notice, CreatedAt: at(4), DeliveredBy: 13},
		},
	}

	got := assemble(parent, h)

	type step struct {
		Source transcript.Source
		Time   time.Time
		Body   transcript.Body
	}
	fromCommand := func(task protocol.TaskID, id uint64) transcript.Source {
		return transcript.Source{TaskID: task, CommandID: id}
	}
	fromMessage := func(id uint64) transcript.Source { return transcript.Source{TaskID: parent, MessageID: id} }
	want := []step{
		{fromCommand(parent, 10), at(0), transcript.OwnerPrompt{Text: "Plan."}},
		{transcript.Source{TaskID: parent, Seq: 1}, at(1), transcript.HarnessStarted{PID: 7}},
		{fromCommand(child, 11), at(2), transcript.ChildSpawned{Child: child, Prompt: "Say PEAR."}},
		{fromMessage(1), at(3), transcript.MessageSent{To: child, Text: "Go."}},
		{fromMessage(3), at(4), transcript.ChildEnded{Child: other, State: "failed"}},
		{fromCommand(parent, 13), at(5), transcript.MessageReceived{From: &child, Text: "PEAR"}},
		{fromCommand(parent, 13), at(5), transcript.MessageReceived{Text: notice}},
		{fromCommand(parent, 14), at(6), transcript.MessageReceived{From: &child, Text: "Message from task child: lost"}},
		{fromCommand(parent, 15), at(7), transcript.OwnerPrompt{Text: "Carry on."}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for idx := range want {
		g := step{got[idx].Source, got[idx].Time, got[idx].Body}
		if !reflect.DeepEqual(g, want[idx]) {
			t.Errorf("entry %d = %+v\nwant      %+v", idx, g, want[idx])
		}
	}
}

func TestGivenParentAndChildThatMessageWhenReadingTheirTranscriptsThenEachShowsItsSide(t *testing.T) {
	srv := startTestServer(t)
	parent := taskIn(t, srv.store, "parent", TaskRunning)
	child := spawned(t, srv.store, "parent", "child")
	child.drive(TaskRunning)
	parent.event(protocol.KindHarnessExited, cleanly, TaskFinished)
	sendFrom(t, srv.store, "child", "parent", "PEAR")
	other := spawned(t, srv.store, "parent", "other")
	other.drive(TaskFailed)

	kindsOf := func(task protocol.TaskID) []transcript.Kind {
		t.Helper()
		entries, err := srv.Transcript(t.Context(), task)
		if err != nil {
			t.Fatal(err)
		}
		var kinds []transcript.Kind
		for _, entry := range entries {
			if entry.Body.Kind() != transcript.KindHarnessStarted && entry.Body.Kind() != transcript.KindHarnessExited {
				kinds = append(kinds, entry.Body.Kind())
			}
		}
		return kinds
	}

	wantParent := []transcript.Kind{transcript.KindOwnerPrompt, transcript.KindChildSpawned, transcript.KindMessageReceived,
		transcript.KindChildSpawned, transcript.KindChildEnded}
	if got := kindsOf("parent"); !reflect.DeepEqual(got, wantParent) {
		t.Errorf("parent's kinds = %v, want %v", got, wantParent)
	}
	if got, want := kindsOf("child"), []transcript.Kind{transcript.KindOwnerPrompt, transcript.KindMessageSent}; !reflect.DeepEqual(got, want) {
		t.Errorf("child's kinds = %v, want %v", got, want)
	}
}
