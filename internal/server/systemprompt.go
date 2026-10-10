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
	purposeRule +
	"Only these two tools reach other tasks; no other messaging or agent tool does. " +
	"A message reaches its recipient as its next prompt once its current turn has ended, " +
	"so to wait for a child's result or a reply, end your turn."

// purposeRule tells an agent allowed spawn_task what the purpose it must
// give is for (docs/adr/2026-10-10-projects-and-lineage.md).
const purposeRule = "spawn_task requires a purpose: one line saying why the child exists and what you expect back from it. " +
	"The owner sees it wherever the child is listed, and the child reads it at the top of its prompt. "

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
			"and returns the child's task id. " + purposeRule + "Only this tool reaches other tasks; no other messaging or agent tool does. " +
			"A child's report reaches you as your next prompt once your current turn has ended, so to wait for it, end your turn."
	case send:
		return intro + "The orchestrator MCP server gives you one tool for this: send_message sends text to another task by its id. " +
			"Only this tool reaches other tasks; no other messaging or agent tool does. " +
			"A message reaches its recipient as its next prompt once its current turn has ended, so to wait for a reply, end your turn."
	}
	return intro + "You have no tool that reaches other tasks, and no other messaging or agent tool reaches them either."
}

// promptParts are what a task's system prompt is composed of. Parent is
// the task's parent, nil for the owner's task; Tools the gateway tools it
// is allowed, as protocol.StartTask.Tools has them; Agents those it may be
// told of, which it is when Tools let it spawn. Agent is its agent
// definition's system prompt, Project its project's instructions and
// Task what the owner's request adds, each empty for none.
type promptParts struct {
	Parent  *protocol.TaskID
	Tools   []string
	Agents  []Agent
	Agent   string
	Project string
	Task    string
}

// systemPrompt composes a task's system prompt
// (docs/adr/2026-10-07-task-interface.md,
// docs/adr/2026-10-09-agents-and-placement.md,
// docs/adr/2026-10-10-projects-and-lineage.md): how to reach other tasks
// with the gateway tools the task is allowed; for a child, its parent and
// its duty to report; the agents it may spawn; then the agent's system
// prompt, the project's instructions and the request's, so that the
// agent's role comes before the project's conventions. Empty parts are
// left out.
func systemPrompt(p promptParts) string {
	parts := []string{toolsPrompt(p.Tools)}
	if p.Parent != nil {
		parent, tools := *p.Parent, p.Tools
		report := fmt.Sprintf("You are a child task of task %[1]s, which waits for your result. "+
			"When you have it, either send it to task %[1]s with the orchestrator's send_message tool, "+
			"or end your turn with it as your final reply: if you end a turn without having sent a message during it, "+
			"your final reply is handed back to task %[1]s as your report.", parent)
		if tools != nil && !slices.Contains(tools, protocol.ToolSendMessage) {
			report = fmt.Sprintf("You are a child task of task %[1]s, which waits for your result. "+
				"When you end your turn, your final reply is handed back to task %[1]s as your report, "+
				"so end it with your result.", parent)
		}
		parts = append(parts, report)
	}
	for _, part := range []string{spawnable(p.Tools, p.Agents), p.Agent, p.Project, p.Task} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n\n")
}

// oneLine is text with its runs of whitespace, line breaks included, made
// single spaces, and trimmed: a purpose as it is stored and shown.
func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}

// withPurpose is prompt with purpose, when it is not empty, at its top as
// a paragraph of its own (docs/adr/2026-10-10-projects-and-lineage.md).
func withPurpose(purpose, prompt string) string {
	if purpose == "" {
		return prompt
	}
	return "Purpose: " + purpose + "\n\n" + prompt
}
