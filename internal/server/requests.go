package server

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// maxAgentRequestBytes bounds an agent's request, which carries at most
// a prompt or a message.
const maxAgentRequestBytes = 1 << 20

// postAgentRequest carries out a request an agent of the path's task made
// through its daemon (docs/adr/2026-10-08-inbox-delivery.md), and answers
// with what the agent is told. The task must be assigned to the daemon,
// or the request gets 403, and must have a process running, or it gets
// 409. A request the server refuses gets 422 with the reason, for the
// agent to read.
func (s *Server) postAgentRequest(w http.ResponseWriter, r *http.Request) {
	daemon, ok := daemonFromPath(w, r)
	if !ok {
		return
	}
	task, err := protocol.ParseTaskID(r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var request protocol.AgentRequest
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxAgentRequestBytes), &request); err != nil {
		http.Error(w, "decode request: "+err.Error(), http.StatusBadRequest)
		return
	}
	var reply any
	switch request.Kind {
	case protocol.AgentSpawn:
		var spawn protocol.Spawn
		if err := decodeStrict(bytes.NewReader(request.Payload), &spawn); err != nil {
			http.Error(w, "decode spawn: "+err.Error(), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(spawn.Prompt) == "" {
			http.Error(w, "the child task needs a prompt", http.StatusBadRequest)
			return
		}
		// rand.Text uses only letters and digits, so the id is always valid.
		var command protocol.Command
		command, err = s.store.spawnTask(r.Context(), daemon, task, protocol.TaskID(rand.Text()), spawn)
		reply = protocol.Spawned{TaskID: command.TaskID}
	case protocol.AgentSend:
		var send protocol.Send
		if err := decodeStrict(bytes.NewReader(request.Payload), &send); err != nil {
			http.Error(w, "decode send: "+err.Error(), http.StatusBadRequest)
			return
		}
		if _, err := protocol.ParseTaskID(string(send.To)); err != nil {
			http.Error(w, fmt.Sprintf("%q is not a task id", send.To), http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(send.Text) == "" {
			http.Error(w, "the message needs text", http.StatusBadRequest)
			return
		}
		var delivered bool
		delivered, err = s.store.sendMessage(r.Context(), daemon, task, send)
		reply = protocol.Sent{Delivered: delivered}
	default:
		http.Error(w, fmt.Sprintf("unknown request kind %q", request.Kind), http.StatusBadRequest)
		return
	}
	if foreign, ok := errors.AsType[*foreignTaskError](err); ok {
		http.Error(w, foreign.Error(), http.StatusForbidden)
		return
	}
	switch {
	case errors.Is(err, errNotRunning):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errRefused):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case err != nil:
		s.internalError(w, err)
	default:
		writeJSON(w, http.StatusOK, reply)
	}
}
