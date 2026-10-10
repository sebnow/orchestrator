package claude

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// Message types the normaliser reads content blocks from.
const (
	TypeAssistant = "assistant"
	TypeUser      = "user"
)

// contentBlock is one block of an assistant or user message's content.
// Each block type fills only its own fields.
type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	// ToolUseID, Content and IsError belong to tool_result. Content is a
	// string or an array of blocks.
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// Normalise turns the payload of one harness_output event written by
// Claude Code into transcript bodies, in the order the line holds them.
//
// Assistant text, thinking and tool_use blocks, user tool_result blocks
// and result messages have bodies of their own. Two message types yield
// nothing: system/init, which restates the session's settings at the
// start of every turn, and rate_limit_event, which the daemon reports in
// neutral form as a quota_observed event of its own. Everything else,
// including a line that is not stream-json, becomes transcript.Unknown.
//
// An assistant or user message that a subagent wrote carries the id of
// the tool call that started the subagent in parent_tool_use_id
// (https://code.claude.com/docs/en/headless.md, "Follow subagent
// messages"; docs/design/2026-10-09-subagent-stream.md records a run);
// every body made from it carries that id as its ParentToolUseID.
//
// Normalise reads each line on its own, so the turn ends it makes carry
// no context size; a Normaliser reading a task's lines in order gives
// them one.
func Normalise(payload json.RawMessage) []transcript.Body {
	return new(Normaliser).Normalise(payload)
}

// Normaliser normalises the lines of one task in order, as Normalise
// does each line, and remembers what the session's context size was
// after the main conversation's latest model call, which a result alone
// does not say: its usage sums every call of the turn
// (spikes/mod-vs-stdout/runs/control-1: cache_read_input_tokens 35,496
// in the first result, the sum of the turn's two calls, 14,167 and
// 21,329).
type Normaliser struct {
	contextTokens int64
}

// Normalise turns one line, the next of its task, into transcript
// bodies, as the package's Normalise does, and gives the end of a turn
// the context size of the turn's last call.
func (n *Normaliser) Normalise(payload json.RawMessage) []transcript.Body {
	msg, err := Parse(payload)
	if err != nil {
		return []transcript.Body{unknownLine(payload)}
	}
	if _, ok := msg.Init(); ok {
		return nil
	}
	if _, ok := msg.RateLimit(); ok {
		return nil
	}
	if result, ok := msg.Result(); ok {
		ended := turnEnded(msg.Subtype, result)
		ended.ContextTokens = n.contextTokens
		return []transcript.Body{ended}
	}
	if msg.Type == TypeAssistant && parentToolUseID(payload) == "" {
		if tokens := callContext(payload); tokens > 0 {
			n.contextTokens = tokens
		}
	}
	var bodies []transcript.Body
	switch msg.Type {
	case TypeAssistant:
		if blocks, ok := messageBlocks(payload); ok {
			bodies = assistantBodies(blocks)
		}
	case TypeUser:
		if blocks, ok := messageBlocks(payload); ok {
			bodies = userBodies(blocks)
		}
	}
	if bodies == nil {
		bodies = []transcript.Body{unknown(lineType(msg.Type, msg.Subtype), payload)}
	}
	if parent := parentToolUseID(payload); parent != "" {
		for idx, body := range bodies {
			bodies[idx] = withParent(body, parent)
		}
	}
	return bodies
}

// callContext is the context of the model call an assistant line comes
// from: the input it read, from the cache or not, and what it wrote to
// the cache; 0 when the line carries no usage. Claude Code writes one
// line per content block, each with the call's usage.
func callContext(line json.RawMessage) int64 {
	var msg struct {
		Message struct {
			Usage Usage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return 0
	}
	usage := msg.Message.Usage
	return usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
}

// parentToolUseID returns the line's parent_tool_use_id, which is null
// for the main conversation's messages.
func parentToolUseID(line json.RawMessage) string {
	var env struct {
		ParentToolUseID *string `json:"parent_tool_use_id"`
	}
	if err := json.Unmarshal(line, &env); err != nil || env.ParentToolUseID == nil {
		return ""
	}
	return *env.ParentToolUseID
}

// withParent returns body marked as written by the subagent that the tool
// call parent started.
func withParent(body transcript.Body, parent string) transcript.Body {
	switch b := body.(type) {
	case transcript.AgentText:
		b.ParentToolUseID = parent
		return b
	case transcript.AgentThinking:
		b.ParentToolUseID = parent
		return b
	case transcript.ToolCall:
		b.ParentToolUseID = parent
		return b
	case transcript.ToolResult:
		b.ParentToolUseID = parent
		return b
	case transcript.Unknown:
		b.ParentToolUseID = parent
		return b
	}
	return body
}

// turnEnded is the end of the turn a result reports. Its context window
// is the largest of the models the process used, which is the main
// model's unless a subagent ran a model with a larger window.
func turnEnded(subtype string, result Result) transcript.TurnEnded {
	var window int64
	for _, usage := range result.ModelUsage {
		window = max(window, usage.ContextWindow)
	}
	return transcript.TurnEnded{
		ContextWindow:            window,
		IsError:                  result.IsError,
		Outcome:                  subtype,
		StopReason:               result.StopReason,
		NumTurns:                 result.NumTurns,
		Duration:                 time.Duration(result.DurationMS) * time.Millisecond,
		TotalCostUSD:             result.TotalCostUSD,
		InputTokens:              result.Usage.InputTokens,
		OutputTokens:             result.Usage.OutputTokens,
		CacheCreationInputTokens: result.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     result.Usage.CacheReadInputTokens,
	}
}

// messageBlocks returns the content blocks of an assistant or user
// message, each with its raw JSON. It reports false when the content is
// not an array of blocks, such as a user prompt given as a plain string.
func messageBlocks(line json.RawMessage) ([]rawBlock, bool) {
	var msg struct {
		Message struct {
			Content []json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil || msg.Message.Content == nil {
		return nil, false
	}
	blocks := make([]rawBlock, 0, len(msg.Message.Content))
	for _, raw := range msg.Message.Content {
		var block contentBlock
		if err := json.Unmarshal(raw, &block); err != nil {
			return nil, false
		}
		blocks = append(blocks, rawBlock{contentBlock: block, raw: raw})
	}
	return blocks, true
}

type rawBlock struct {
	contentBlock
	raw json.RawMessage
}

func assistantBodies(blocks []rawBlock) []transcript.Body {
	bodies := make([]transcript.Body, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			bodies = append(bodies, transcript.AgentText{Text: block.Text})
		case "thinking":
			bodies = append(bodies, transcript.AgentThinking{Text: block.Thinking})
		case "redacted_thinking":
			bodies = append(bodies, transcript.AgentThinking{})
		case "tool_use":
			bodies = append(bodies, transcript.ToolCall{ID: block.ID, Name: block.Name, Input: block.Input})
		default:
			bodies = append(bodies, unknown(lineType(TypeAssistant, block.Type), block.raw))
		}
	}
	return bodies
}

// userBodies keeps the tool results of a user message. Its other blocks,
// such as the "[Request interrupted by user for tool use]" text Claude
// Code writes after an interrupt, are passed on as unknown.
func userBodies(blocks []rawBlock) []transcript.Body {
	bodies := make([]transcript.Body, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "tool_result" {
			bodies = append(bodies, unknown(lineType(TypeUser, block.Type), block.raw))
			continue
		}
		bodies = append(bodies, transcript.ToolResult{
			ToolCallID: block.ToolUseID,
			Content:    toolResultText(block.Content),
			IsError:    block.IsError,
		})
	}
	return bodies
}

// toolResultText renders a tool_result's content as text. Content is a
// string, or an array of blocks whose text blocks are joined by newlines
// and whose other blocks, such as images, are shown by type.
func toolResultText(content json.RawMessage) string {
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		return text
	}
	var blocks []contentBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return string(content)
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		} else {
			parts = append(parts, "["+block.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

// unknownLine describes a line Parse refused, naming its type when the
// line has one.
func unknownLine(payload json.RawMessage) transcript.Unknown {
	var env struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	json.Unmarshal(payload, &env)
	return unknown(lineType(env.Type, env.Subtype), payload)
}

func unknown(typ string, raw json.RawMessage) transcript.Unknown {
	return transcript.Unknown{RecordKind: string(protocol.KindHarnessOutput), Type: typ, Raw: raw}
}

// lineType is "type/subtype", or "type" when there is no subtype.
func lineType(typ, subtype string) string {
	if subtype == "" {
		return typ
	}
	return typ + "/" + subtype
}
