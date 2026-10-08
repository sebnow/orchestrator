package protocol

import "encoding/json"

// AgentRequestKind says what an agent asks the server and what the
// request's payload holds.
type AgentRequestKind string

const (
	// AgentSpawn: Spawn, answered with Spawned.
	AgentSpawn AgentRequestKind = "spawn"
	// AgentSend: Send, answered with Sent.
	AgentSend AgentRequestKind = "send"
)

// AgentRequest is a request an agent makes of the server through its
// daemon (docs/adr/2026-10-08-inbox-delivery.md). The daemon POSTs it to
// /v1/daemons/{daemon}/tasks/{task}/requests, where {task} is the agent's
// task, and relays the answer, or the error text of a response that is
// not 2xx, to the agent.
type AgentRequest struct {
	Kind    AgentRequestKind `json:"kind"`
	Payload json.RawMessage  `json:"payload"`
}

// Spawn starts a child task with Prompt. An empty Model leaves the choice
// to the server.
type Spawn struct {
	Prompt string `json:"prompt"`
	Model  string `json:"model,omitempty"`
}

// Spawned names the child task a Spawn started.
type Spawned struct {
	TaskID TaskID `json:"task_id"`
}

// Send sends Text to the task To.
type Send struct {
	To   TaskID `json:"to"`
	Text string `json:"text"`
}

// Sent says what became of a Send: Delivered is true when the message was
// issued to its recipient at once as a prompt, and false when it waits in
// the recipient's inbox.
type Sent struct {
	Delivered bool `json:"delivered"`
}
