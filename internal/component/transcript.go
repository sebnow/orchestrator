package component

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// Transcript is a task's transcript, in order. New entries are appended
// to it as they arrive.
func Transcript(entries []transcript.Entry) html.Node {
	return html.El("ol", attrs("id", "transcript", "class", "transcript"), TranscriptEntries(entries, nil))
}

// TranscriptEntries are entries, to append to a Transcript that shows
// shown already. An entry a harness subagent wrote is nested under the
// tool call that started the subagent: in that call's list when the call
// is among entries, appended to the list on the page out of band when
// the call is among shown, and otherwise in a list of its own, titled by
// the call's id, where its first entry falls.
func TranscriptEntries(entries, shown []transcript.Entry) html.Node {
	onPage := make(map[string]bool)
	for _, entry := range shown {
		if call, ok := entry.Body.(transcript.ToolCall); ok {
			onPage[call.ID] = true
		}
		if parent := transcript.ParentToolUseID(entry.Body); parent != "" {
			onPage[parent] = true
		}
	}
	calls := make(map[string]bool)
	nested := make(map[string][]transcript.Entry)
	for _, entry := range entries {
		if call, ok := entry.Body.(transcript.ToolCall); ok {
			calls[call.ID] = true
		}
		if parent := transcript.ParentToolUseID(entry.Body); parent != "" {
			nested[parent] = append(nested[parent], entry)
		}
	}
	var render func(entry transcript.Entry) html.Node
	renderAll := func(entries []transcript.Entry) []html.Node {
		nodes := make([]html.Node, len(entries))
		for idx, entry := range entries {
			nodes[idx] = render(entry)
		}
		return nodes
	}
	render = func(entry transcript.Entry) html.Node {
		call, ok := entry.Body.(transcript.ToolCall)
		if !ok {
			return transcriptEntry(entry, nil)
		}
		return transcriptEntry(entry, subagent(call.ID, subagentTitle(call), renderAll(nested[call.ID])...))
	}

	var nodes []html.Node
	var appended []string
	toAppend := make(map[string][]html.Node)
	orphans := make(map[string]bool)
	for _, entry := range entries {
		parent := transcript.ParentToolUseID(entry.Body)
		switch {
		case parent == "":
			nodes = append(nodes, render(entry))
		case onPage[parent]:
			if toAppend[parent] == nil {
				appended = append(appended, parent)
			}
			toAppend[parent] = append(toAppend[parent], render(entry))
		case calls[parent]:
			// Rendered inside its call.
		case !orphans[parent]:
			orphans[parent] = true
			nodes = append(nodes, html.El("li", attrs("class", "entry from-agent"),
				subagent(parent, "Subagent of tool call "+parent, renderAll(nested[parent])...)))
		}
	}
	for _, parent := range appended {
		nodes = append(nodes, html.El("ol", attrs("hx-swap-oob", "beforeend:#"+subagentListID(parent)), toAppend[parent]...))
	}
	return html.Fragment(nodes...)
}

// subagent is the list of what the subagent that the tool call id started
// wrote, folded under title. The list is there, empty, for every tool
// call, so that entries arriving later can be appended to it; the style
// sheet hides it while it is empty.
func subagent(id, title string, entries ...html.Node) html.Node {
	return html.El("details", attrs("class", "subagent", "open", ""),
		html.El("summary", nil, html.Text(title)),
		html.El("ol", attrs("id", subagentListID(id), "class", "transcript"), entries...))
}

// subagentListID is the element id of the list nested under tool call id.
// Tool call ids are the harness's, so they are hex-encoded to make a
// valid id and selector whatever they hold.
func subagentListID(id string) string {
	return "subagent-" + hex.EncodeToString([]byte(id))
}

// subagentTitle names the subagent a tool call started by the call's
// tool and, when its input has one, its description.
func subagentTitle(call transcript.ToolCall) string {
	var input struct {
		Description string `json:"description"`
	}
	title := "Subagent started by " + call.Name
	if json.Unmarshal(call.Input, &input) == nil && input.Description != "" {
		title += ": " + input.Description
	}
	return title
}

// TranscriptEntry is one entry. Text the agent or a tool wrote keeps its
// whitespace and is not interpreted, as markdown or otherwise; tool
// inputs and results are folded away. A tool call has an empty list for
// the entries of a subagent it starts.
func TranscriptEntry(entry transcript.Entry) html.Node {
	if call, ok := entry.Body.(transcript.ToolCall); ok {
		return transcriptEntry(entry, subagent(call.ID, subagentTitle(call)))
	}
	return transcriptEntry(entry, nil)
}

// transcriptEntry is entry, with nested after its body.
func transcriptEntry(entry transcript.Entry, nested html.Node) html.Node {
	// subject follows label in the entry's header, for links to other
	// tasks.
	from, label, subject, body := "daemon", "", html.Node(nil), html.Node(nil)
	switch b := entry.Body.(type) {
	case transcript.OwnerPrompt:
		from, label = "owner", "Owner prompted"
		switch {
		case b.Resume:
			label = "Owner resumed the task"
		case b.SpawnedBy != nil:
			from, label = "task", "Spawned by parent task "
			subject, body = link(taskURL(string(*b.SpawnedBy)), string(*b.SpawnedBy)), preformatted(b.Text)
		default:
			body = preformatted(b.Text)
		}
	case transcript.PauseRequested:
		from, label = "owner", "Owner asked the agent to pause"
	case transcript.StopRequested:
		from, label = "owner", "Owner stopped the task"
	case transcript.Interrupted:
		from, label = "owner", "Owner interrupted the turn"
	case transcript.PermissionAnswered:
		from, label = "owner", "Owner denied request "+b.RequestID
		if b.Allow {
			label = "Owner allowed request " + b.RequestID
		}
		if b.By == transcript.AnsweredByPolicy {
			from, label = "daemon", "Request "+b.RequestID+" denied by policy"
			if b.Allow {
				label = "Request " + b.RequestID + " allowed by policy"
			}
		}
		if b.Message != "" {
			body = preformatted(b.Message)
		}
	case transcript.AgentText:
		from, label, body = "agent", "Agent", preformatted(b.Text)
	case transcript.AgentThinking:
		from, label = "agent", "Agent thought"
		if b.Text != "" {
			body = Details("Thinking", preformatted(b.Text))
		}
	case transcript.ToolCall:
		from, label = "agent", "Agent called "+b.Name
		body = Details("Input", preformatted(prettyJSON(b.Input)))
	case transcript.ToolResult:
		from, label = "agent", "Tool returned"
		if b.IsError {
			label = "Tool failed"
		}
		body = Details("Result", preformatted(b.Content))
	case transcript.TurnEnded:
		from, label = "agent", "Turn ended: "+b.Outcome
		body = html.El("p", nil, html.Text(fmt.Sprintf("stop reason %s, %d turns, %s, %s so far, %d tokens in, %d out, %d cache written, %d cache read",
			orNone(b.StopReason), b.NumTurns, b.Duration.Round(time.Millisecond), cost(b.TotalCostUSD),
			b.InputTokens, b.OutputTokens, b.CacheCreationInputTokens, b.CacheReadInputTokens)))
	case transcript.PermissionRequested:
		from, label = "agent", "Agent asked to run "+b.Tool
		body = Details("Input", preformatted(prettyJSON(b.Input)))
	case transcript.PauseAcknowledged:
		from, label, body = "agent", "Agent acknowledged the pause", preformatted(b.Note)
	case transcript.PauseSettled:
		label = "Pause took effect"
		if b.Interrupted {
			label = "Pause took effect after an interrupt"
		}
	case transcript.QuotaObserved:
		label, body = "Usage limits", QuotaReadout(protocol.QuotaObserved(b), time.Time{})
	case transcript.HarnessStarted:
		label = "Harness started"
		body = html.El("p", nil, html.Text(fmt.Sprintf("pid %d, model %s, in %s", b.PID, orNone(b.Model), b.Workdir)))
	case transcript.HarnessExited:
		label = "Harness exited with code " + strconv.Itoa(b.ExitCode)
		body = html.Fragment(paragraph(b.Error), stderr(b.Stderr))
		if b.CutShortBy != "" {
			next := "Resume continues its session."
			if b.NewSession {
				next = "No session was recorded, so Resume starts a new session with the task's first prompt."
			}
			label = "The daemon " + b.CutShortBy + " during the turn; the task is paused"
			if b.CutShortBy == "interrupted" {
				label = "The owner interrupted the turn; the task is paused"
			}
			body = html.Fragment(paragraph(next), stderr(b.Stderr))
		}
	case transcript.MessageSent:
		from, label = "agent", "Agent sent a message to task "
		subject, body = link(taskURL(string(b.To)), string(b.To)), preformatted(b.Text)
	case transcript.MessageReceived:
		from, label, body = "task", "Notice from the orchestrator", preformatted(b.Text)
		if b.From != nil {
			label, subject = "Message from task ", link(taskURL(string(*b.From)), string(*b.From))
		}
	case transcript.ChildSpawned:
		from, label = "agent", "Agent started child task "
		subject, body = link(taskURL(string(b.Child)), string(b.Child)), preformatted(b.Prompt)
	case transcript.ChildEnded:
		label = "Child task "
		subject = html.Fragment(link(taskURL(string(b.Child)), string(b.Child)), html.Text(" ended as "+b.State))
	case transcript.MessageUndeliverable:
		label = "Task "
		subject = html.Fragment(link(taskURL(string(b.To)), string(b.To)),
			html.Text(" ended as "+b.State+" before the agent's messages reached it; they were not delivered"))
	case transcript.BranchPushed:
		label = "Pushed branch " + b.Branch + " at " + shortCommit(b.Commit)
		if b.Error != "" {
			label = "Could not push branch " + b.Branch + " at " + shortCommit(b.Commit)
		}
		body = html.Fragment(paragraph(branchCounts(b.Ahead, b.Uncommitted)), errorText(b.Error))
	case transcript.TaskMoved:
		label = "Daemon " + string(b.From) + " was lost; the task started afresh on daemon " + string(b.To)
		body = preformatted(b.Prompt)
	case transcript.Unknown:
		label = "Unrecognised " + b.RecordKind
		if b.Type != "" {
			label += " of type " + b.Type
		}
		body = Details("Payload", preformatted(string(b.Raw)))
	default:
		label = "Unrecognised entry " + string(entry.Body.Kind())
	}
	return html.El("li", attrs("class", "entry from-"+from),
		html.El("header", nil, timestamp(entry.Time), html.Text(" "+label), subject),
		body, nested)
}

func paragraph(text string) html.Node {
	if text == "" {
		return nil
	}
	return html.El("p", nil, html.Text(text))
}

func stderr(text string) html.Node {
	if text == "" {
		return nil
	}
	return Details("Standard error", preformatted(text))
}

func orNone(text string) string {
	if text == "" {
		return "none"
	}
	return text
}

// prettyJSON indents raw, or returns it as it is when it is not JSON.
func prettyJSON(raw json.RawMessage) string {
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return string(raw)
	}
	return out.String()
}

// shortCommit abbreviates a commit id as git does by default, or more
// when the id is short already.
func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// branchCounts says how far a pushed branch is beyond the task's start
// and how many files the workspace left uncommitted.
func branchCounts(ahead, uncommitted int) string {
	text := plural(ahead, "commit") + " beyond the start"
	if uncommitted > 0 {
		text += "; " + plural(uncommitted, "file") + " left uncommitted in the workspace"
	}
	return text
}

func errorText(text string) html.Node {
	if text == "" {
		return nil
	}
	return html.El("p", attrs("class", "problem"), html.Text(text))
}
