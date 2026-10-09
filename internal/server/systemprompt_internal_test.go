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
		{nil, []string{"spawn_task", "send_message"}, nil},
		{[]string{protocol.ToolSpawnTask}, []string{"spawn_task"}, []string{"send_message"}},
		{[]string{protocol.ToolSendMessage}, []string{"send_message"}, []string{"spawn_task"}},
		{[]string{}, []string{"no tool that reaches other tasks"}, []string{"spawn_task", "send_message"}},
	} {
		got := systemPrompt(&parent, tc.tools, "Be brief.")

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
