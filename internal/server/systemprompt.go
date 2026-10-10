package server

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// The server composes the part of every task's system prompt that
// describes the orchestrator's own mechanisms, from the task's tools,
// its parent and its workspace
// (docs/adr/2026-10-10-agent-models-and-capacity.md, "Mechanics in the
// prompt"): which tools reach other tasks, how spawn_task and
// send_message work, the agents a spawner may name, a child's parent and
// purpose, and how a workspace's work is delivered. An agent's own
// system prompt carries only its role. The paragraphs name the gateway's
// tools, so they change when those are renamed.

// purposeRule tells an agent allowed spawn_task what the purpose it must
// give is for (docs/adr/2026-10-10-projects-and-lineage.md).
const purposeRule = "spawn_task requires a purpose: one line saying why the child exists and what you expect back from it. " +
	"The owner sees it wherever the child is listed, and the child reads it at the top of its prompt. "

// toolsPrompt says which of the gateway's tools reach other tasks, for an
// agent allowed tools, as protocol.StartTask.Tools has them: nil allows
// both. It says that no other tool reaches a task, because a harness may
// have messaging and agent tools of its own with similar names, which
// reach other sessions on the machine rather than tasks.
func toolsPrompt(tools []string) string {
	spawn, send := allows(tools, protocol.ToolSpawnTask), allows(tools, protocol.ToolSendMessage)
	const intro = "You run as a task of an orchestrator, which can run other agents as tasks alongside you. "
	switch {
	case spawn && send:
		return intro + "The orchestrator MCP server gives you two tools that reach other tasks, spawn_task and send_message. " +
			"Only these two reach other tasks; no other messaging or agent tool does."
	case spawn:
		return intro + "The orchestrator MCP server gives you one tool that reaches other tasks, spawn_task. " +
			"Only it reaches other tasks; no other messaging or agent tool does."
	case send:
		return intro + "The orchestrator MCP server gives you one tool that reaches other tasks, send_message. " +
			"Only it reaches other tasks; no other messaging or agent tool does."
	}
	return intro + "You have no tool that reaches other tasks, and no other messaging or agent tool reaches them either."
}

// allows reports whether tools, as protocol.StartTask.Tools has them,
// allow tool.
func allows(tools []string, tool string) bool {
	return tools == nil || slices.Contains(tools, tool)
}

// spawnPrompt tells a task allowed spawn_task how it works; agents says
// whether there are agents to list. A task with a repository is told
// that a child works in a clone of its own.
func spawnPrompt(workspace, agents bool) string {
	text := "spawn_task starts a child task on a prompt you write, as no agent, when the child gets your own tools and settings, "
	if agents {
		text += "or as one of the agents listed below, named as its agent, "
	}
	text += "and returns the child's task id. " + purposeRule
	if workspace {
		text += "A child works in a fresh clone of your repository, on a branch of its own, so its commits do not appear in your workspace. "
	}
	return text + "The child's report reaches you as your next prompt once the turn in which you spawned it has ended, " +
		"so after spawning, end your turn to wait for it. Do not poll or wait for the child within the turn: it cannot report while your turn runs."
}

// sendPrompt tells a task allowed send_message what it reaches.
const sendPrompt = "send_message sends text to another task by its id: your parent, a child you spawned, " +
	"or any other task whose id you know, unless it has stopped or failed. " +
	"A message reaches its recipient as its next prompt once its current turn has ended, " +
	"or, if it is paused, once the owner resumes it; messages to you reach you the same way. " +
	"To wait for a reply, end your turn."

// childPrompt tells a child task who spawned it, for what purpose, and
// that its final reply is its report to parent, unless it sends one with
// send_message, which tools allow or not.
func childPrompt(parent protocol.TaskID, purpose string, tools []string) string {
	text := fmt.Sprintf("You are a child task of task %s, which waits for your result.", parent)
	if purpose != "" {
		text = fmt.Sprintf("You are a child task of task %s, which spawned you for this purpose: %s", parent, purpose)
		if !strings.HasSuffix(purpose, ".") {
			text += "."
		}
	}
	if allows(tools, protocol.ToolSendMessage) {
		return text + fmt.Sprintf(" When you have your result, either send it to task %[1]s with the orchestrator's send_message tool, "+
			"or end your turn with it as your final reply: if you end a turn without having sent a message during it, "+
			"your final reply is handed back to task %[1]s as your report.", parent)
	}
	return text + fmt.Sprintf(" When you end your turn, your final reply is handed back to task %s as your report, "+
		"so end it with your result.", parent)
}

// workspacePrompt tells the agent of task, which has a repository, how
// its work leaves the workspace (docs/adr/2026-10-08-work-delivery.md)
// and what the clone's origin is
// (docs/adr/2026-10-10-daemon-push-identity.md). The daemon makes the
// clone, its branch and its push; the server words them, as part of the
// one composition of the orchestrator's mechanisms, from the branch
// name protocol.TaskBranch gives both.
func workspacePrompt(task protocol.TaskID) string {
	branch := protocol.TaskBranch(task)
	return "Your working directory is a git clone of the task's repository, checked out on branch " + branch + ". " +
		"Commit your work to " + branch + " as you go, in commits with clear messages, and stay on that branch. " +
		"Never push: the orchestrator pushes " + branch + " for you at the end of every turn, without force, " +
		"so do not rewrite commits it has pushed; a hook in the clone refuses any other push. " +
		"Work you leave uncommitted is not delivered. " +
		"The clone's origin is the daemon's mirror of the repository, not the repository itself: " +
		"fetching from origin brings what the mirror held when the daemon last updated it, which it does when it prepares a workspace."
}

// promptParts are what a task's system prompt is composed of. ID is the
// task's id, and Workspace says it has a repository. Parent is the task's
// parent, nil for the owner's task, and Purpose why the parent spawned
// it, empty for none. Tools are the gateway tools it is allowed, as
// protocol.StartTask.Tools has them; Agents those it may be told of,
// which it is when Tools let it spawn. Agent is its agent definition's
// system prompt, Project its project's instructions and Task what the
// owner's request adds, each empty for none.
type promptParts struct {
	ID        protocol.TaskID
	Workspace bool
	Parent    *protocol.TaskID
	Purpose   string
	Tools     []string
	Agents    []Agent
	Agent     string
	Project   string
	Task      string
}

// systemPrompt composes a task's system prompt
// (docs/adr/2026-10-07-task-interface.md,
// docs/adr/2026-10-09-agents-and-placement.md,
// docs/adr/2026-10-10-projects-and-lineage.md,
// docs/adr/2026-10-10-agent-models-and-capacity.md), a paragraph each:
// which tools reach other tasks; for a task allowed spawn_task, how it
// works and the agents it may name; for one allowed send_message, what
// it reaches; for a child, its parent, its purpose and its duty to
// report; for a task with a repository, its workspace and branch; then
// the agent's system prompt, the project's instructions and the
// request's, so that the agent's role comes before the project's
// conventions. Empty parts are left out.
func systemPrompt(p promptParts) string {
	parts := []string{toolsPrompt(p.Tools)}
	if allows(p.Tools, protocol.ToolSpawnTask) {
		parts = append(parts, spawnPrompt(p.Workspace, len(p.Agents) > 0), agentsPrompt(p.Agents))
	}
	if allows(p.Tools, protocol.ToolSendMessage) {
		parts = append(parts, sendPrompt)
	}
	if p.Parent != nil {
		parts = append(parts, childPrompt(*p.Parent, p.Purpose, p.Tools))
	}
	if p.Workspace {
		parts = append(parts, workspacePrompt(p.ID))
	}
	parts = append(parts, p.Agent, p.Project, p.Task)
	return strings.Join(slices.DeleteFunc(parts, func(part string) bool { return part == "" }), "\n\n")
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
