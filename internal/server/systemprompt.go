package server

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// messagingPrompt tells an agent allowed every gateway tool how to reach
// other tasks (docs/adr/2026-10-08-inbox-delivery.md). It names the
// daemon's gateway tools, so it changes when they are renamed. It says
// that no other tool reaches a task, because a harness may have
// messaging tools of its own with similar names.
const messagingPrompt = "You run as a task of an orchestrator, which can run other agents as tasks alongside you. " +
	"The orchestrator MCP server gives you two tools for this: spawn_task starts a child task on a prompt you write " +
	"and returns the child's task id, and send_message sends text to another task by its id. " +
	"Only these two tools reach other tasks; no other messaging or agent tool does. " +
	"A message reaches its recipient as its next prompt once its current turn has ended, " +
	"so to wait for a child's result or a reply, end your turn."

// toolsPrompt is messagingPrompt for an agent allowed the gateway tools
// tools, as protocol.StartTask.Tools has them: nil allows both.
func toolsPrompt(tools []string) string {
	spawn := tools == nil || slices.Contains(tools, protocol.ToolSpawnTask)
	send := tools == nil || slices.Contains(tools, protocol.ToolSendMessage)
	const intro = "You run as a task of an orchestrator, which can run other agents as tasks alongside you. "
	switch {
	case spawn && send:
		return messagingPrompt
	case spawn:
		return intro + "The orchestrator MCP server gives you one tool for this: spawn_task starts a child task on a prompt you write " +
			"and returns the child's task id. Only this tool reaches other tasks; no other messaging or agent tool does. " +
			"A child's report reaches you as your next prompt once your current turn has ended, so to wait for it, end your turn."
	case send:
		return intro + "The orchestrator MCP server gives you one tool for this: send_message sends text to another task by its id. " +
			"Only this tool reaches other tasks; no other messaging or agent tool does. " +
			"A message reaches its recipient as its next prompt once its current turn has ended, so to wait for a reply, end your turn."
	}
	return intro + "You have no tool that reaches other tasks, and no other messaging or agent tool reaches them either."
}

// systemPrompt composes a task's system prompt
// (docs/adr/2026-10-07-task-interface.md): how to reach other tasks with
// the gateway tools the task is allowed; for a child, its parent and its
// duty to report; then each of extra that is not empty, such as what the
// owner gave.
func systemPrompt(parent *protocol.TaskID, tools []string, extra ...string) string {
	parts := []string{toolsPrompt(tools)}
	if parent != nil {
		report := fmt.Sprintf("You are a child task of task %[1]s, which waits for your result. "+
			"When you have it, either send it to task %[1]s with the orchestrator's send_message tool, "+
			"or end your turn with it as your final reply: if you end a turn without having sent a message during it, "+
			"your final reply is handed back to task %[1]s as your report.", *parent)
		if tools != nil && !slices.Contains(tools, protocol.ToolSendMessage) {
			report = fmt.Sprintf("You are a child task of task %[1]s, which waits for your result. "+
				"When you end your turn, your final reply is handed back to task %[1]s as your report, "+
				"so end it with your result.", *parent)
		}
		parts = append(parts, report)
	}
	for _, part := range extra {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n\n")
}
