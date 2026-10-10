package server

import (
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

func TestGivenAllowedToolsWhenComposingTheSystemPromptThenItNamesOnlyThoseAndAChildWithoutSendMessageIsNotToldToUseIt(t *testing.T) {
	parent := protocol.TaskID("parent-1")
	for _, tc := range []struct {
		tools        []string
		names, lacks []string
	}{
		{nil, []string{"spawn_task", "send_message", purposeRule}, nil},
		{[]string{protocol.ToolSpawnTask}, []string{"spawn_task", purposeRule}, []string{"send_message"}},
		{[]string{protocol.ToolSendMessage}, []string{"send_message"}, []string{"spawn_task", "purpose"}},
		{[]string{}, []string{"no tool that reaches other tasks"}, []string{"spawn_task", "send_message", "purpose"}},
	} {
		got := systemPrompt(promptParts{Parent: &parent, Tools: tc.tools, Task: "Be brief."})

		for _, want := range tc.names {
			if !strings.Contains(got, want) {
				t.Errorf("tools %q: prompt lacks %q:\n%s", tc.tools, want, got)
			}
		}
		for _, lack := range tc.lacks {
			if strings.Contains(got, lack) {
				t.Errorf("tools %q: prompt names %q:\n%s", tc.tools, lack, got)
			}
		}
		if !strings.Contains(got, "You are a child task of task parent-1") || !strings.HasSuffix(got, "\n\nBe brief.") {
			t.Errorf("tools %q: prompt lacks the parent or the owner's part:\n%s", tc.tools, got)
		}
	}
}

func TestGivenEveryPartWhenComposingTheSystemPromptThenTheyComeInOrderEachAParagraphAndEmptyOnesAreLeftOut(t *testing.T) {
	parent := protocol.TaskID("parent-1")
	agents := []Agent{{Name: "reviewer", Description: "Reviews."}}

	got := systemPrompt(promptParts{Parent: &parent, Agents: agents, Agent: "AGENT-ROLE", Project: "PROJECT-CONVENTIONS", Task: "TASK-EXTRA"})

	if !strings.HasPrefix(got, toolsPrompt(nil)+"\n\n") {
		t.Errorf("prompt does not start with the tools paragraph:\n%s", got)
	}
	last := 0
	for _, part := range []string{
		"You are a child task of task parent-1",
		"You can start a child task as one of these agents",
		"AGENT-ROLE", "PROJECT-CONVENTIONS", "TASK-EXTRA",
	} {
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
