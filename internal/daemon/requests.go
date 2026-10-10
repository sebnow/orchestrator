package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const (
	// agentRequestTimeout bounds forwarding one agent request, so that a
	// server that does not answer does not hold the agent's tool call.
	agentRequestTimeout = 30 * time.Second
	// maxAgentReplyBytes bounds the server's reply to an agent request.
	maxAgentReplyBytes = 1 << 20
)

// forwarder forwards an agent request for task to the server and returns
// the server's reply. The error's text is meant for the agent.
type forwarder func(ctx context.Context, task protocol.TaskID, request protocol.AgentRequest) (json.RawMessage, error)

// forwardTo returns a forwarder that POSTs requests to the server at
// base as daemon, with client.
func forwardTo(client *http.Client, base *url.URL, daemon protocol.DaemonID) forwarder {
	return func(ctx context.Context, task protocol.TaskID, request protocol.AgentRequest) (json.RawMessage, error) {
		body, err := json.Marshal(request)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(ctx, agentRequestTimeout)
		defer cancel()
		target := base.JoinPath("v1", "daemons", string(daemon), "tasks", string(task), "requests")
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("the orchestrator could not be reached: %w", err)
		}
		defer resp.Body.Close()
		reply, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentReplyBytes))
		if err != nil {
			return nil, fmt.Errorf("the orchestrator's reply was cut off: %w", err)
		}
		if resp.StatusCode/100 != 2 {
			return nil, fmt.Errorf("the orchestrator refused (%s): %s", resp.Status, strings.TrimSpace(string(reply)))
		}
		return reply, nil
	}
}

// errNotConnected answers an agent request on a daemon that runs tasks
// without a server.
var errNotConnected = errors.New("this daemon is not connected to an orchestrator")

// spawnTaskInput is spawn_task's input. Purpose, like Prompt, has no
// omitempty, so the schema the MCP SDK infers requires it.
type spawnTaskInput struct {
	Purpose string `json:"purpose" jsonschema:"one line saying why the child exists and what you expect back from it; the owner sees it wherever the child is listed, and the child reads it at the top of its prompt"`
	Prompt  string `json:"prompt" jsonschema:"the child task's instructions; it sees nothing else of your conversation"`
	Agent   string `json:"agent,omitempty" jsonschema:"the name of the agent to start the child as, from the agents your instructions list; none when omitted"`
	Model   string `json:"model,omitempty" jsonschema:"the model the child runs, such as haiku; the agent's, or yours, when omitted"`
	// Requires is passed on as given: an empty object requires nothing,
	// in place of the agent's labels.
	Requires map[string]string `json:"requires,omitempty" jsonschema:"labels, such as {\"gpu\": \"nvidia\"}, that the daemon the child runs on must have; the agent's when omitted"`
}

type sendMessageInput struct {
	To   string `json:"to" jsonschema:"the id of the task to send the message to"`
	Text string `json:"text" jsonschema:"the message"`
}

// spawnTask serves the gateway's spawn_task tool for task.
func (d *Daemon) spawnTask(task protocol.TaskID) func(context.Context, spawnTaskInput) (string, error) {
	return func(ctx context.Context, in spawnTaskInput) (string, error) {
		var spawned protocol.Spawned
		if err := d.request(ctx, task, protocol.AgentSpawn, protocol.Spawn{Purpose: in.Purpose, Prompt: in.Prompt, Model: in.Model, Agent: in.Agent, Requires: in.Requires}, &spawned); err != nil {
			return "", err
		}
		return spawnedText(spawned), nil
	}
}

// spawnedText tells the spawning agent how the child spawned reports: by
// send_message when its tools include it, or else by the hand-back of its
// final reply when its turn ends.
func spawnedText(spawned protocol.Spawned) string {
	if spawned.Tools == nil || slices.Contains(spawned.Tools, protocol.ToolSendMessage) {
		return fmt.Sprintf("Started child task %s. It works on its own and sends its result with %s. "+
			"To wait for it, end your turn; its message arrives as your next prompt.", spawned.TaskID, SendMessageTool)
	}
	return fmt.Sprintf("Started child task %s. It works on its own; when it finishes, its final report is delivered to you. "+
		"To wait for it, end your turn; the report arrives as your next prompt.", spawned.TaskID)
}

// sendMessage serves the gateway's send_message tool for task.
func (d *Daemon) sendMessage(task protocol.TaskID) func(context.Context, sendMessageInput) (string, error) {
	return func(ctx context.Context, in sendMessageInput) (string, error) {
		var sent protocol.Sent
		if err := d.request(ctx, task, protocol.AgentSend, protocol.Send{To: protocol.TaskID(in.To), Text: in.Text}, &sent); err != nil {
			return "", err
		}
		if sent.Delivered {
			return fmt.Sprintf("Sent. The message is queued for delivery to task %s as its next prompt, which may wait for a free slot or for budget.", in.To), nil
		}
		return fmt.Sprintf("Sent. The message waits in task %s's inbox and reaches it as a prompt once its current turn has ended.", in.To), nil
	}
}

// request forwards an agent request of kind with payload and decodes the
// server's reply into reply.
func (d *Daemon) request(ctx context.Context, task protocol.TaskID, kind protocol.AgentRequestKind, payload, reply any) error {
	if d.forward == nil {
		return errNotConnected
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	data, err := d.forward(ctx, task, protocol.AgentRequest{Kind: kind, Payload: raw})
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, reply); err != nil {
		return fmt.Errorf("the orchestrator's reply is not understood: %w", err)
	}
	return nil
}
