package component

import (
	"bytes"
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
	return html.El("ol", attrs("id", "transcript", "class", "transcript"), TranscriptEntries(entries))
}

// TranscriptEntries are entries side by side, to append to a Transcript.
func TranscriptEntries(entries []transcript.Entry) html.Node {
	nodes := make([]html.Node, len(entries))
	for idx, entry := range entries {
		nodes[idx] = TranscriptEntry(entry)
	}
	return html.Fragment(nodes...)
}

// TranscriptEntry is one entry. Text the agent or a tool wrote keeps its
// whitespace and is not interpreted, as markdown or otherwise; tool
// inputs and results are folded away.
func TranscriptEntry(entry transcript.Entry) html.Node {
	// subject follows label in the entry's header, for links to other
	// tasks.
	from, label, subject, body := "daemon", "", html.Node(nil), html.Node(nil)
	switch b := entry.Body.(type) {
	case transcript.OwnerPrompt:
		from, label = "owner", "Owner prompted"
		if b.Resume {
			label = "Owner resumed the task"
		} else {
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
		if b.Restarted {
			next := "Resume continues its session."
			if b.NewSession {
				next = "No session was recorded, so Resume starts a new session with the task's first prompt."
			}
			label = "The daemon restarted during the turn; the task is paused"
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
		body)
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
