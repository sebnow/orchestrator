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

// Spawn starts a child task with Prompt, as the agent named Agent when
// that is set (docs/adr/2026-10-09-agents-and-placement.md). Purpose is
// one line saying why the child exists and what the spawner expects back
// (docs/adr/2026-10-10-projects-and-lineage.md); the server refuses a
// spawn without one. An empty Model leaves the choice to the server.
// Requires, when not nil, are the labels the child's daemon must have, in
// place of the agent's; an empty one requires nothing.
type Spawn struct {
	Purpose  string            `json:"purpose"`
	Prompt   string            `json:"prompt"`
	Model    string            `json:"model,omitempty"`
	Agent    string            `json:"agent,omitempty"`
	Requires map[string]string `json:"requires,omitzero"`
}

// Spawned names the child task a Spawn started. Tools are the gateway
// tools the child may call, as in StartTask.Tools: nil, as from a server
// that predates the field, for every one of them, and empty for none.
type Spawned struct {
	TaskID TaskID   `json:"task_id"`
	Tools  []string `json:"tools,omitzero"`
}

// Send sends Text to the task To.
type Send struct {
	To   TaskID `json:"to"`
	Text string `json:"text"`
}

// Sent says what became of a Send: Delivered is true when the message is
// to be its recipient's next turn, which the server admits as its
// scheduling allows (docs/adr/2026-10-08-scheduling.md), and false when
// it waits in the recipient's inbox for the recipient's turn to end.
type Sent struct {
	Delivered bool `json:"delivered"`
}
