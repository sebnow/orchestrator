package claude

import "encoding/json"

type userMessage struct {
	Type    string      `json:"type"`
	Message userContent `json:"message"`
	UUID    string      `json:"uuid,omitempty"`
}

type userContent struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// EncodeUserMessage returns the stdin line, newline included, that sends
// text as a user prompt. A non-empty uuid is echoed back as
// user_message_uuid by the turn that answers the prompt.
//
// The line has no priority field. Sent while a turn runs, such a message
// reaches the model in that turn once its running tool calls finish; sent
// between turns, it starts the next one.
//
// The SDK reference declares parent_tool_use_id and session_id on user
// messages; this line omits both, which Claude Code 2.1.289 accepted in
// the spike runs. That omission is undocumented.
func EncodeUserMessage(text, uuid string) []byte {
	return encodeLine(userMessage{
		Type:    "user",
		Message: userContent{Role: "user", Content: text},
		UUID:    uuid,
	})
}

type controlRequest struct {
	Type      string         `json:"type"`
	RequestID string         `json:"request_id"`
	Request   controlSubtype `json:"request"`
}

type controlSubtype struct {
	Subtype string `json:"subtype"`
}

// EncodeInterrupt returns the stdin line, newline included, that
// interrupts the running turn. Claude Code answers with a
// control_response carrying requestID, then ends the turn with an error
// result; the session stays alive.
//
// The control protocol is documented for Agent SDK clients, not on the
// headless page.
func EncodeInterrupt(requestID string) []byte {
	return encodeLine(controlRequest{
		Type:      "control_request",
		RequestID: requestID,
		Request:   controlSubtype{Subtype: "interrupt"},
	})
}

func encodeLine(v any) []byte {
	line, err := json.Marshal(v)
	if err != nil {
		// Only strings are encoded, which cannot fail to marshal.
		panic(err)
	}
	return append(line, '\n')
}
