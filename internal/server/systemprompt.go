package server

import (
	"fmt"
	"strings"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// messagingPrompt tells every agent how to reach other tasks
// (docs/adr/2026-10-08-inbox-delivery.md). It names the daemon's gateway
// tools, so it changes when they are renamed. It says that no other tool
// reaches a task, because a harness may have messaging tools of its own
// with similar names.
const messagingPrompt = "You run as a task of an orchestrator, which can run other agents as tasks alongside you. " +
	"The orchestrator MCP server gives you two tools for this: spawn_task starts a child task on a prompt you write " +
	"and returns the child's task id, and send_message sends text to another task by its id. " +
	"Only these two tools reach other tasks; no other messaging or agent tool does. " +
	"A message reaches its recipient as its next prompt once its current turn has ended, " +
	"so to wait for a child's result or a reply, end your turn."

// systemPrompt composes a task's system prompt
// (docs/adr/2026-10-07-task-interface.md): how to reach other tasks; for
// a child, its parent and its duty to report; then what the owner gave,
// if anything.
func systemPrompt(parent *protocol.TaskID, owners string) string {
	parts := []string{messagingPrompt}
	if parent != nil {
		parts = append(parts, fmt.Sprintf("You are a child task of task %[1]s, which waits for your result. "+
			"When you have it, send it to task %[1]s with the orchestrator's send_message tool before you end your turn; "+
			"task %[1]s does not see your replies otherwise.", *parent))
	}
	if owners != "" {
		parts = append(parts, owners)
	}
	return strings.Join(parts, "\n\n")
}
