package server

import (
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// paragraphs of the orchestrator's mechanisms, by what each starts with.
const (
	spawnParagraph     = "spawn_task starts a child task"
	agentsParagraph    = "You can start a child task as one of these agents"
	sendParagraph      = "send_message sends text to another task"
	childParagraph     = "You are a child task of task parent-1"
	workspaceParagraph = "Your working directory is a git clone"
)

func TestGivenEachCombinationOfToolsParentAndWorkspaceWhenComposingThenExactlyTheirMechanicsAreDescribed(t *testing.T) {
	parent := protocol.TaskID("parent-1")
	agents := []Agent{{Name: "scout", Description: "Reads\nand searches."}}
	every := []string{spawnParagraph, agentsParagraph, sendParagraph, childParagraph, workspaceParagraph}
	for _, tools := range [][]string{nil, {protocol.ToolSpawnTask}, {protocol.ToolSendMessage}, {}} {
		for _, child := range []bool{false, true} {
			for _, workspace := range []bool{false, true} {
				p := promptParts{ID: "task-1", Workspace: workspace, Tools: tools, Agents: agents}
				if child {
					p.Parent, p.Purpose = &parent, "Check the parser"
				}

				got := systemPrompt(p)

				want := map[string]bool{
					spawnParagraph:     allows(tools, protocol.ToolSpawnTask),
					agentsParagraph:    allows(tools, protocol.ToolSpawnTask),
					sendParagraph:      allows(tools, protocol.ToolSendMessage),
					childParagraph:     child,
					workspaceParagraph: workspace,
				}
				for _, paragraph := range every {
					has := strings.HasPrefix(got, paragraph) || strings.Contains(got, "\n\n"+paragraph)
					if has != want[paragraph] {
						t.Errorf("tools %q, child %v, workspace %v: has %q = %v, want %v:\n%s", tools, child, workspace, paragraph, has, want[paragraph], got)
					}
				}
			}
		}
	}
}

func TestGivenSpawnerWhenComposingThenItIsToldTheAgentsThePurposeAndToEndItsTurnRatherThanPoll(t *testing.T) {
	got := systemPrompt(promptParts{ID: "task-1", Workspace: true, Tools: []string{protocol.ToolSpawnTask},
		Agents: []Agent{{Name: "scout", Description: "Reads\nand searches."}, {Name: "junior"}}})

	requireContains(t, got, purposeRule, "named as its agent", "returns the child's task id",
		"A child works in a fresh clone of your repository, on a branch of its own",
		"reaches you as your next prompt once the turn in which you spawned it has ended", "Do not poll",
		"- scout: Reads and searches.\n- junior")
	requireLacks(t, got, "send_message")
	if bare := systemPrompt(promptParts{Tools: []string{protocol.ToolSpawnTask}}); strings.Contains(bare, "listed below") || strings.Contains(bare, "fresh clone") {
		t.Errorf("a spawner with no agents and no repository is told of either:\n%s", bare)
	}
}

func TestGivenChildWhenComposingThenItIsToldItsParentItsPurposeAndThatItsFinalReplyIsItsReport(t *testing.T) {
	parent := protocol.TaskID("parent-1")

	messaging := systemPrompt(promptParts{Parent: &parent, Purpose: "Check the parser.", Tools: nil})
	silent := systemPrompt(promptParts{Parent: &parent, Tools: []string{}})

	requireContains(t, messaging, "You are a child task of task parent-1, which spawned you for this purpose: Check the parser. ",
		"send it to task parent-1 with the orchestrator's send_message tool", "handed back to task parent-1 as your report")
	requireContains(t, silent, "You are a child task of task parent-1, which waits for your result. ",
		"When you end your turn, your final reply is handed back to task parent-1 as your report")
	requireLacks(t, silent, "send_message", "spawn_task")
}

func TestGivenTaskWithARepositoryWhenComposingThenItIsToldItsBranchThePushAndThatOriginIsTheMirror(t *testing.T) {
	got := systemPrompt(promptParts{ID: "task-1", Workspace: true, Tools: []string{}})

	requireContains(t, got, "checked out on branch orchestrator/task-1.", "Never push: the orchestrator pushes orchestrator/task-1 for you at the end of every turn",
		"a hook in the clone refuses any other push", "Work you leave uncommitted is not delivered",
		"The clone's origin is the daemon's mirror of the repository")
}

func TestGivenEveryPartWhenComposingTheSystemPromptThenTheyComeInOrderEachAParagraphAndEmptyOnesAreLeftOut(t *testing.T) {
	parent := protocol.TaskID("parent-1")
	agents := []Agent{{Name: "reviewer", Description: "Reviews."}}

	got := systemPrompt(promptParts{ID: "task-1", Workspace: true, Parent: &parent, Agents: agents, Agent: "AGENT-ROLE", Project: "PROJECT-CONVENTIONS", Task: "TASK-EXTRA"})

	if !strings.HasPrefix(got, toolsPrompt(nil)+"\n\n") {
		t.Errorf("prompt does not start with the tools paragraph:\n%s", got)
	}
	last := 0
	for _, part := range []string{spawnParagraph, agentsParagraph, sendParagraph, childParagraph, workspaceParagraph, "AGENT-ROLE", "PROJECT-CONVENTIONS", "TASK-EXTRA"} {
		idx := strings.Index(got, "\n\n"+part)
		if idx <= last {
			t.Errorf("paragraph %q at %d, want one after %d:\n%s", part, idx, last, got)
		}
		last = idx
	}
	if !strings.HasSuffix(got, "\n\nTASK-EXTRA") {
		t.Errorf("prompt does not end with the task's part:\n%s", got)
	}
	if bare := systemPrompt(promptParts{Tools: []string{}, Project: "PROJECT-CONVENTIONS"}); bare != toolsPrompt([]string{})+"\n\nPROJECT-CONVENTIONS" {
		t.Errorf("prompt with only project instructions = %q", bare)
	}
}

func TestGivenAllowedToolsWhenTheirParagraphIsComposedThenItNamesOnlyThose(t *testing.T) {
	for _, tc := range []struct {
		tools        []string
		names, lacks []string
	}{
		{nil, []string{"spawn_task", "send_message"}, nil},
		{[]string{protocol.ToolSpawnTask}, []string{"spawn_task"}, []string{"send_message"}},
		{[]string{protocol.ToolSendMessage}, []string{"send_message"}, []string{"spawn_task"}},
		{[]string{}, []string{"no tool that reaches other tasks"}, []string{"spawn_task", "send_message"}},
	} {
		got := toolsPrompt(tc.tools)
		for _, want := range tc.names {
			if !strings.Contains(got, want) {
				t.Errorf("tools %q: paragraph lacks %q: %s", tc.tools, want, got)
			}
		}
		for _, lack := range tc.lacks {
			if strings.Contains(got, lack) {
				t.Errorf("tools %q: paragraph names %q: %s", tc.tools, lack, got)
			}
		}
	}
}
