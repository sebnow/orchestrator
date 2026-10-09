package server

import (
	"reflect"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

func agentSays(text string) string {
	return `{"type":"assistant","message":{"content":[{"type":"text","text":"` + text + `"}]},"parent_tool_use_id":null}`
}

func TestGivenChildThatSentNothingWhenItsTurnFinishesThenItsLastTextIsHandedBackToItsParentAsItsNextPrompt(t *testing.T) {
	store, _ := openTestStore(t)
	parent := taskIn(t, store, "parent", TaskRunning)
	child := spawned(t, store, "parent", "child")
	parent.event(protocol.KindHarnessExited, cleanly, TaskFinished)

	child.event(protocol.KindHarnessStarted, started, TaskRunning)
	child.event(protocol.KindHarnessOutput, agentSays("Looking."), TaskRunning)
	child.event(protocol.KindHarnessOutput, `{"type":"assistant","message":{"content":[{"type":"text","text":"SUBAGENT"}]},"parent_tool_use_id":"toolu_1"}`, TaskRunning)
	child.event(protocol.KindHarnessOutput, agentSays("PEAR"), TaskRunning)
	child.event(protocol.KindHarnessOutput, `{"type":"assistant","message":{"content":[{"type":"text","text":"SUBAGENT-LAST"}]},"parent_tool_use_id":"toolu_1"}`, TaskRunning)
	child.event(protocol.KindHarnessExited, cleanly, TaskFinished)

	requirePrompts(t, prompts(t, store, "parent"), []protocol.Prompt{{Text: "Report from child task child, its final reply as it ended its turn: PEAR", From: fromTask("child")}})
	entries := transcriptOf(t, store, "parent")
	if !hasBody(entries, transcript.MessageReceived{From: fromTask("child"), Text: "PEAR", HandBack: true}) {
		t.Errorf("parent's transcript lacks the hand-back: %+v", entries)
	}
	if !hasBody(transcriptOf(t, store, "child"), transcript.MessageSent{To: "parent", Text: "PEAR", HandBack: true}) {
		t.Errorf("child's transcript lacks the hand-back")
	}
}

func TestGivenChildThatWroteNoTextWhenItsTurnFinishesThenItsParentIsToldItFinishedWithoutAny(t *testing.T) {
	store, _ := openTestStore(t)
	parent := taskIn(t, store, "parent", TaskRunning)
	child := spawned(t, store, "parent", "child")
	parent.event(protocol.KindHarnessExited, cleanly, TaskFinished)

	child.drive(TaskFinished)

	requirePrompts(t, prompts(t, store, "parent"), []protocol.Prompt{{
		Text: "Report from child task child, its final reply as it ended its turn: (Task child finished its turn without writing any text.)", From: fromTask("child"),
	}})
}

func TestGivenChildThatSentAMessageThisTurnWhenItsTurnFinishesThenNothingMoreIsSent(t *testing.T) {
	store, _ := openTestStore(t)
	parent := taskIn(t, store, "parent", TaskRunning)
	child := spawned(t, store, "parent", "child")
	parent.event(protocol.KindHarnessExited, cleanly, TaskFinished)
	child.event(protocol.KindHarnessStarted, started, TaskRunning)
	sendFrom(t, store, "child", "parent", "APPLE")
	child.event(protocol.KindHarnessOutput, agentSays("Sent it."), TaskRunning)

	child.event(protocol.KindHarnessExited, cleanly, TaskFinished)

	requirePrompts(t, prompts(t, store, "parent"), []protocol.Prompt{{Text: "Message from task child: APPLE", From: fromTask("child")}})
}

func TestGivenChildThatReportedOnAnEarlierTurnWhenALaterTurnFinishesSilentlyThenThatTurnIsHandedBack(t *testing.T) {
	store, _ := openTestStore(t)
	parent := taskIn(t, store, "parent", TaskRunning)
	child := spawned(t, store, "parent", "child")
	parent.event(protocol.KindHarnessExited, cleanly, TaskFinished)
	child.event(protocol.KindHarnessStarted, started, TaskRunning)
	sendFrom(t, store, "child", "parent", "APPLE")
	child.event(protocol.KindHarnessExited, cleanly, TaskFinished)
	parent.event(protocol.KindHarnessStarted, started, TaskRunning)
	parent.event(protocol.KindHarnessExited, cleanly, TaskFinished)

	child.command(protocol.CommandPrompt, `{"text":"Again."}`, TaskRunning)
	child.event(protocol.KindHarnessStarted, started, TaskRunning)
	child.event(protocol.KindHarnessOutput, agentSays("PLUM"), TaskRunning)
	child.event(protocol.KindHarnessExited, cleanly, TaskFinished)

	got := prompts(t, store, "parent")
	if len(got) != 2 || got[1].Text != "Report from child task child, its final reply as it ended its turn: PLUM" {
		t.Errorf("parent's prompts = %+v, want APPLE then the hand-back of PLUM", got)
	}
}

func TestGivenOwnersTaskWhenItsTurnFinishesSilentlyThenNothingIsHandedBack(t *testing.T) {
	store, _ := openTestStore(t)
	task := taskIn(t, store, "task", TaskRunning)
	task.event(protocol.KindHarnessOutput, agentSays("Done."), TaskRunning)

	task.event(protocol.KindHarnessExited, cleanly, TaskFinished)

	var count int
	if err := store.db.QueryRowContext(t.Context(), `SELECT count(*) FROM messages`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d messages recorded, want none", count)
	}
}

func transcriptOf(t *testing.T, store *Store, task protocol.TaskID) []transcript.Entry {
	t.Helper()
	h, err := store.taskHistory(t.Context(), task)
	if err != nil {
		t.Fatal(err)
	}
	return assemble(task, h)
}

func hasBody(entries []transcript.Entry, want transcript.Body) bool {
	for _, entry := range entries {
		if reflect.DeepEqual(entry.Body, want) {
			return true
		}
	}
	return false
}
