package server

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

const (
	started  = `{"pid":1}`
	cleanly  = `{"exit_code":0}`
	settled  = `{"interrupted":false}`
	nonZero  = `{"exit_code":1}`
	askToRun = `{"request_id":"r1","tool":"Bash","input":{}}`
)

// taskIn creates task on the daemon laptop and drives it into state.
func taskIn(t *testing.T, store *Store, task protocol.TaskID, state TaskState) *lifecycle {
	t.Helper()
	l := newLifecycle(t, store, task)
	l.drive(state)
	return l
}

// drive takes a pending task into state.
func (l *lifecycle) drive(state TaskState) {
	l.t.Helper()
	if state == TaskPending {
		return
	}
	l.event(protocol.KindHarnessStarted, started, TaskRunning)
	switch state {
	case TaskRunning:
	case TaskAwaitingPermission:
		l.event(protocol.KindPermissionRequested, askToRun, TaskAwaitingPermission)
	case TaskPausing:
		l.command(protocol.CommandPause, "", TaskPausing)
	case TaskPaused:
		l.command(protocol.CommandPause, "", TaskPausing)
		l.event(protocol.KindPauseSettled, settled, TaskPaused)
		l.event(protocol.KindHarnessExited, cleanly, TaskPaused)
	case TaskFinished:
		l.event(protocol.KindHarnessExited, cleanly, TaskFinished)
	case TaskStopped:
		l.event(protocol.KindHarnessExited, cleanly, TaskFinished)
		l.command(protocol.CommandStop, "", TaskStopped)
	case TaskFailed:
		l.event(protocol.KindHarnessExited, nonZero, TaskFailed)
	default:
		l.t.Fatalf("cannot drive a task to %s", state)
	}
}

// prompts returns the prompts issued to task, in order.
func prompts(t *testing.T, store *Store, task protocol.TaskID) []protocol.Prompt {
	t.Helper()
	h, err := store.taskHistory(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	var out []protocol.Prompt
	for _, command := range h.commands {
		if command.Kind != protocol.CommandPrompt {
			continue
		}
		var prompt protocol.Prompt
		if err := json.Unmarshal(command.Payload, &prompt); err != nil {
			t.Fatal(err)
		}
		out = append(out, prompt)
	}
	return out
}

// waiting counts the messages waiting in task's inbox.
func waiting(t *testing.T, store *Store, task protocol.TaskID) int {
	t.Helper()
	var count int
	err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM messages WHERE to_task = ? AND delivered_command_id IS NULL`, string(task)).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func sendFrom(t *testing.T, store *Store, from, to protocol.TaskID, text string) bool {
	t.Helper()
	delivered, err := store.sendMessage(t.Context(), "laptop", from, protocol.Send{To: to, Text: text})
	if err != nil {
		t.Fatalf("send %q from %s to %s: %v", text, from, to, err)
	}
	admitTurns(t, store)
	return delivered
}

func fromTask(task protocol.TaskID) *protocol.TaskID { return &task }

func requirePrompts(t *testing.T, got, want []protocol.Prompt) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		t.Errorf("prompts = %s\nwant      %s", gotJSON, wantJSON)
	}
}

func TestGivenFinishedRecipientWhenATaskSendsThenTheMessageIsIssuedAtOnceAsAPromptFromTheSender(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "sender", TaskRunning)
	taskIn(t, store, "recipient", TaskFinished)

	delivered := sendFrom(t, store, "sender", "recipient", "PEAR")

	if !delivered {
		t.Error("delivered = false, want true")
	}
	requirePrompts(t, prompts(t, store, "recipient"), []protocol.Prompt{{Text: "Message from task sender: PEAR", From: fromTask("sender")}})
	if state := readProgress(t, store, "recipient").State; state != TaskRunning {
		t.Errorf("recipient state = %s, want running", state)
	}
	if n := waiting(t, store, "recipient"); n != 0 {
		t.Errorf("%d messages still wait", n)
	}
}

func TestGivenBusyRecipientWhenTasksSendThenTheMessagesWaitUntilItFinishesAndArriveAsOnePrompt(t *testing.T) {
	for _, state := range []TaskState{TaskPending, TaskRunning, TaskAwaitingPermission, TaskPausing} {
		t.Run(string(state), func(t *testing.T) {
			store, _ := openTestStore(t)
			taskIn(t, store, "first", TaskRunning)
			taskIn(t, store, "second", TaskRunning)
			recipient := taskIn(t, store, "recipient", state)

			if sendFrom(t, store, "first", "recipient", "one") || sendFrom(t, store, "second", "recipient", "two") {
				t.Error("delivered = true, want the messages to wait")
			}
			if got := prompts(t, store, "recipient"); len(got) != 0 {
				t.Fatalf("prompts before the turn ended = %+v", got)
			}
			if state == TaskPending {
				recipient.event(protocol.KindHarnessStarted, started, TaskRunning)
			}
			recipient.event(protocol.KindHarnessExited, cleanly, TaskRunning)

			requirePrompts(t, prompts(t, store, "recipient"), []protocol.Prompt{{
				Text: "Message from task first: one\n\nMessage from task second: two",
				From: fromTask("first"),
			}})
			if n := waiting(t, store, "recipient"); n != 0 {
				t.Errorf("%d messages still wait", n)
			}
		})
	}
}

func TestGivenPausedRecipientWhenATaskSendsThenTheMessageWaitsUntilTheResumedTurnFinishes(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "sender", TaskRunning)
	recipient := taskIn(t, store, "recipient", TaskPaused)

	if sendFrom(t, store, "sender", "recipient", "PEAR") {
		t.Error("delivered = true, want the message to wait")
	}
	if got := prompts(t, store, "recipient"); len(got) != 0 {
		t.Fatalf("prompts while paused = %+v", got)
	}
	recipient.command(protocol.CommandResume, "", TaskRunning)
	recipient.event(protocol.KindHarnessStarted, started, TaskRunning)
	if got := prompts(t, store, "recipient"); len(got) != 0 {
		t.Fatalf("prompts during the resumed turn = %+v", got)
	}
	recipient.event(protocol.KindHarnessExited, cleanly, TaskRunning)

	requirePrompts(t, prompts(t, store, "recipient"), []protocol.Prompt{{Text: "Message from task sender: PEAR", From: fromTask("sender")}})
}

func TestGivenPausingRecipientWhenThePauseSettlesThenItsWaitingMessagesStayUntilTheOwnerResumes(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "sender", TaskRunning)
	recipient := taskIn(t, store, "recipient", TaskPausing)
	sendFrom(t, store, "sender", "recipient", "PEAR")

	recipient.event(protocol.KindPauseSettled, settled, TaskPaused)
	recipient.event(protocol.KindHarnessExited, cleanly, TaskPaused)

	if got := prompts(t, store, "recipient"); len(got) != 0 {
		t.Errorf("prompts to the paused task = %+v", got)
	}
	if n := waiting(t, store, "recipient"); n != 1 {
		t.Errorf("%d messages wait, want 1", n)
	}
}

func TestGivenEndedOrMissingRecipientWhenATaskSendsThenItIsRefusedAndNothingIsKept(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "sender", TaskRunning)
	taskIn(t, store, "stopped", TaskStopped)
	taskIn(t, store, "failed", TaskFailed)

	for _, to := range []protocol.TaskID{"stopped", "failed", "nobody", "sender"} {
		_, err := store.sendMessage(t.Context(), "laptop", "sender", protocol.Send{To: to, Text: "hello"})
		if !errors.Is(err, errRefused) {
			t.Errorf("send to %s: %v, want errRefused", to, err)
		}
	}
	var count int
	if err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM messages`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d messages kept", count)
	}
}

func TestGivenRequestFromATaskWithoutARunningProcessWhenItArrivesThenItIsRefusedAsNotRunning(t *testing.T) {
	for _, state := range []TaskState{TaskFinished, TaskPaused, TaskStopped, TaskFailed} {
		t.Run(string(state), func(t *testing.T) {
			store, _ := openTestStore(t)
			taskIn(t, store, "agent", state)
			taskIn(t, store, "recipient", TaskRunning)

			_, sendErr := store.sendMessage(t.Context(), "laptop", "agent", protocol.Send{To: "recipient", Text: "hello"})
			_, spawnErr := store.spawnTask(t.Context(), "laptop", "agent", "child", protocol.Spawn{Prompt: "Say PEAR."})

			if !errors.Is(sendErr, errNotRunning) || !errors.Is(spawnErr, errNotRunning) {
				t.Errorf("send: %v; spawn: %v; want errNotRunning", sendErr, spawnErr)
			}
		})
	}
}

func TestGivenRequestNamingAnotherDaemonsTaskWhenItArrivesThenItIsRefusedAsForeign(t *testing.T) {
	store, _ := openTestStore(t)
	taskIn(t, store, "agent", TaskRunning)
	if _, err := store.heldSeqs(t.Context(), "vps"); err != nil {
		t.Fatal(err)
	}

	for _, task := range []protocol.TaskID{"agent", "nobody"} {
		_, sendErr := store.sendMessage(t.Context(), "vps", task, protocol.Send{To: "agent", Text: "hello"})
		_, spawnErr := store.spawnTask(t.Context(), "vps", task, "child", protocol.Spawn{Prompt: "Say PEAR."})

		if _, ok := errors.AsType[*foreignTaskError](sendErr); !ok {
			t.Errorf("send as %s: %v, want *foreignTaskError", task, sendErr)
		}
		if _, ok := errors.AsType[*foreignTaskError](spawnErr); !ok {
			t.Errorf("spawn as %s: %v, want *foreignTaskError", task, spawnErr)
		}
	}
}

func TestGivenRunningParentWhenItSpawnsThenTheChildStartsOnItsDaemonWithItsSettings(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.heldSeqs(t.Context(), "laptop"); err != nil {
		t.Fatal(err)
	}
	parentStart := protocol.StartTask{
		Prompt:      "Plan the work.",
		Workspace:   &protocol.Workspace{Repo: "https://example.com/r.git", Ref: "main"},
		Model:       "opus",
		PauseLimits: protocol.PauseLimits{Acknowledge: 2 * time.Minute, Cleanup: 7 * time.Minute},
	}
	parent := ownersTask("parent", "laptop")
	parent.Start = parentStart
	queueTask(t, store, parent)
	admitTurns(t, store)
	(&lifecycle{t: t, store: store, task: "parent"}).drive(TaskRunning)

	turn, err := store.spawnTask(t.Context(), "laptop", "parent", "child", protocol.Spawn{Prompt: "Say PEAR."})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.spawnTask(t.Context(), "laptop", "parent", "other", protocol.Spawn{Prompt: "Say PLUM.", Model: "haiku"}); err != nil {
		t.Fatal(err)
	}
	admitTurns(t, store)

	if turn.Kind != turnStart || turn.TaskID != "child" {
		t.Errorf("turn = %+v, want the start of child", turn)
	}
	commands, err := store.commandsAfter(t.Context(), "laptop", protocol.CommandPosition{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(commands, func(c protocol.Command) bool { return c.TaskID == "child" && c.Kind == protocol.CommandStartTask }) {
		t.Errorf("commands = %+v, want the start of child on laptop", commands)
	}
	child, err := store.task(t.Context(), "child")
	if err != nil {
		t.Fatal(err)
	}
	want := parentStart
	want.Prompt = "Say PEAR."
	want.SystemPrompt = systemPrompt(promptParts{ID: "child", Workspace: want.Workspace != nil, Parent: fromTask("parent")})
	if child.ParentID == nil || *child.ParentID != "parent" || child.DaemonID != "laptop" || child.State != TaskPending || !reflect.DeepEqual(child.Start, want) {
		t.Errorf("child = %+v, start %+v; want a pending child of parent on laptop with %+v", child.taskSummary, child.Start, want)
	}
	other, err := store.task(t.Context(), "other")
	if err != nil {
		t.Fatal(err)
	}
	if other.Model != "haiku" {
		t.Errorf("model of a child that named one = %s, want haiku", other.Model)
	}
}

// spawned spawns child of the running parent and returns its lifecycle.
func spawned(t *testing.T, store *Store, parent, child protocol.TaskID) *lifecycle {
	t.Helper()
	if _, err := store.spawnTask(t.Context(), "laptop", parent, child, protocol.Spawn{Prompt: "Say PEAR."}); err != nil {
		t.Fatal(err)
	}
	admitTurns(t, store)
	return &lifecycle{t: t, store: store, task: child}
}

func TestGivenChildWhenItFailsThenItsFinishedParentIsPromptedWithANoticeAboutTheChild(t *testing.T) {
	store, _ := openTestStore(t)
	parent := taskIn(t, store, "parent", TaskRunning)
	child := spawned(t, store, "parent", "child")
	parent.event(protocol.KindHarnessExited, cleanly, TaskFinished)

	child.drive(TaskFailed)

	requirePrompts(t, prompts(t, store, "parent"), []protocol.Prompt{{
		Text: "Notice from the orchestrator: Your child task child has ended as failed. It will send no more messages.",
		From: fromTask("child"),
	}})
}

func TestGivenChildWhenTheOwnerStopsItWhileItsParentRunsThenTheNoticeWaitsForTheParentsTurnToEnd(t *testing.T) {
	store, _ := openTestStore(t)
	parent := taskIn(t, store, "parent", TaskRunning)
	child := spawned(t, store, "parent", "child")

	child.drive(TaskStopped)

	if got := prompts(t, store, "parent"); len(got) != 0 {
		t.Fatalf("prompts to the running parent = %+v", got)
	}
	parent.event(protocol.KindHarnessExited, cleanly, TaskRunning)
	// The child finished a turn without sending before it was stopped, so
	// its hand-back comes first.
	requirePrompts(t, prompts(t, store, "parent"), []protocol.Prompt{{
		Text: "Report from child task child, its final reply as it ended its turn: (Task child finished its turn without writing any text.)\n\n(Orchestrator: No branch pushed for task child.)\n\n" +
			"Notice from the orchestrator: Your child task child has ended as stopped. It will send no more messages.",
		From: fromTask("child"),
	}})
}

func TestGivenStoppedParentWhenItsChildFailsThenNoNoticeIsKept(t *testing.T) {
	store, _ := openTestStore(t)
	parent := taskIn(t, store, "parent", TaskRunning)
	child := spawned(t, store, "parent", "child")
	parent.command(protocol.CommandStop, "", TaskRunning)
	parent.event(protocol.KindHarnessExited, cleanly, TaskStopped)

	child.drive(TaskFailed)

	if n := waiting(t, store, "parent"); n != 0 {
		t.Errorf("%d messages wait for the stopped parent", n)
	}
}

func TestGivenOwnersTaskAndChildWhenStartedThenEachSystemPromptExplainsMessagingAndTheChildsNamesItsParent(t *testing.T) {
	srv := startTestServer(t)
	if _, err := srv.store.heldSeqs(t.Context(), "laptop"); err != nil {
		t.Fatal(err)
	}
	start := protocol.StartTask{Prompt: "Plan.", SystemPrompt: "Be brief.", PauseLimits: testStart.PauseLimits}
	command, err := srv.startTask(t.Context(), taskRequest{Daemon: "laptop", Start: start})
	if err != nil {
		t.Fatal(err)
	}
	admitTurns(t, srv.store)
	parent := &lifecycle{t: t, store: srv.store, task: command.TaskID}
	parent.drive(TaskRunning)
	child := spawned(t, srv.store, command.TaskID, "child")

	owners, err := srv.store.task(t.Context(), command.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	spawnedChild, err := srv.store.task(t.Context(), child.task)
	if err != nil {
		t.Fatal(err)
	}

	if want := systemPrompt(promptParts{ID: command.TaskID, Task: "Be brief."}); owners.Start.SystemPrompt != want {
		t.Errorf("owner's task system prompt = %q, want %q", owners.Start.SystemPrompt, want)
	}
	spawner := command.TaskID
	wantChild := systemPrompt(promptParts{ID: child.task, Parent: &spawner, Purpose: spawnedChild.Purpose})
	if spawnedChild.Start.SystemPrompt != wantChild || !strings.Contains(wantChild, "You are a child task of task "+string(spawner)) {
		t.Errorf("child's system prompt = %q\nwant %q", spawnedChild.Start.SystemPrompt, wantChild)
	}
}

func TestGivenMessagesWaitingForARecipientWhenItFailsThenEachSenderIsPromptedWithANoticeOfItsUndeliveredMessages(t *testing.T) {
	store, _ := openTestStore(t)
	recipient := taskIn(t, store, "recipient", TaskRunning)
	first := taskIn(t, store, "first", TaskRunning)
	second := taskIn(t, store, "second", TaskRunning)
	sendFrom(t, store, "first", "recipient", "one")
	sendFrom(t, store, "second", "recipient", "two")
	sendFrom(t, store, "first", "recipient", "three")
	first.event(protocol.KindHarnessExited, cleanly, TaskFinished)
	second.event(protocol.KindHarnessExited, cleanly, TaskFinished)

	recipient.event(protocol.KindHarnessExited, nonZero, TaskFailed)

	requirePrompts(t, prompts(t, store, "first"), []protocol.Prompt{{
		Text: "Notice from the orchestrator: Task recipient has ended as failed before your 2 messages reached it, so they were not delivered.",
		From: fromTask("recipient"),
	}})
	requirePrompts(t, prompts(t, store, "second"), []protocol.Prompt{{
		Text: "Notice from the orchestrator: Task recipient has ended as failed before your message reached it, so it was not delivered.",
		From: fromTask("recipient"),
	}})
	h, err := store.taskHistory(t.Context(), "first")
	if err != nil {
		t.Fatal(err)
	}
	var notices []transcript.Body
	for _, entry := range assemble("first", h) {
		if _, ok := entry.Body.(transcript.MessageUndeliverable); ok {
			notices = append(notices, entry.Body)
		}
	}
	if want := []transcript.Body{transcript.MessageUndeliverable{To: "recipient", State: "failed"}}; !reflect.DeepEqual(notices, want) {
		t.Errorf("notices in the sender's transcript = %+v, want %+v", notices, want)
	}
}

func TestGivenParentMessageWaitingForItsChildWhenTheChildIsStoppedThenTheParentGetsOneNoticeSayingBoth(t *testing.T) {
	store, _ := openTestStore(t)
	parent := taskIn(t, store, "parent", TaskRunning)
	child := spawned(t, store, "parent", "child")
	child.event(protocol.KindHarnessStarted, started, TaskRunning)
	sendFrom(t, store, "parent", "child", "more detail")
	parent.event(protocol.KindHarnessExited, cleanly, TaskFinished)

	child.command(protocol.CommandStop, "", TaskRunning)
	child.event(protocol.KindHarnessExited, cleanly, TaskStopped)

	requirePrompts(t, prompts(t, store, "parent"), []protocol.Prompt{{
		Text: "Notice from the orchestrator: Your child task child has ended as stopped. It will send no more messages. " +
			"Your message to it was not delivered.",
		From: fromTask("child"),
	}})
}

func TestGivenSenderThatHasEndedWhenItsRecipientFailsThenNoNoticeIsKept(t *testing.T) {
	store, _ := openTestStore(t)
	recipient := taskIn(t, store, "recipient", TaskRunning)
	sender := taskIn(t, store, "sender", TaskRunning)
	sendFrom(t, store, "sender", "recipient", "one")
	sender.event(protocol.KindHarnessExited, nonZero, TaskFailed)

	recipient.event(protocol.KindHarnessExited, nonZero, TaskFailed)

	if n := waiting(t, store, "sender"); n != 0 {
		t.Errorf("%d messages wait for the failed sender", n)
	}
}
