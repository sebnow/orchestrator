package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// maxOwnerRequestBytes bounds an owner's request body, which carries at
// most a prompt and a system prompt.
const maxOwnerRequestBytes = 1 << 20

// createTaskRequest is the body of POST /v1/tasks: the start_task payload
// and the daemon to run it on.
type createTaskRequest struct {
	DaemonID protocol.DaemonID `json:"daemon_id"`
	protocol.StartTask
}

// commandRequest is the body of POST /v1/tasks/{task}/commands.
type commandRequest struct {
	Kind    protocol.CommandKind `json:"kind"`
	Payload json.RawMessage      `json:"payload"`
}

// postTask creates a task on the named daemon and issues its start_task
// command, which it returns.
func (s *Server) postTask(w http.ResponseWriter, r *http.Request) {
	var request createTaskRequest
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxOwnerRequestBytes), &request); err != nil {
		http.Error(w, "decode task: "+err.Error(), http.StatusBadRequest)
		return
	}
	daemon, err := protocol.ParseDaemonID(string(request.DaemonID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateStart(request.StartTask); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	command, err := s.startTask(r.Context(), daemon, request.StartTask)
	if errors.Is(err, errUnknownDaemon) {
		http.Error(w, err.Error()+": it has not connected yet", http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, command)
}

// startTask creates a task on daemon under a new id, with the default
// model when start names none and the composed system prompt, and returns
// its start_task command.
func (s *Server) startTask(ctx context.Context, daemon protocol.DaemonID, start protocol.StartTask) (protocol.Command, error) {
	if start.Model == "" {
		start.Model = s.defaultModel
	}
	start.SystemPrompt = systemPrompt(nil, start.SystemPrompt)
	// rand.Text uses only letters and digits, so the id is always valid.
	return s.store.createTask(ctx, daemon, protocol.TaskID(rand.Text()), start)
}

func validateStart(start protocol.StartTask) error {
	if start.Prompt == "" {
		return errors.New("prompt is required")
	}
	if start.Workspace != nil && (start.Workspace.Repo == "" || start.Workspace.Ref == "") {
		return errors.New("workspace needs both repo and ref")
	}
	if start.PauseLimits.Acknowledge <= 0 || start.PauseLimits.Cleanup <= 0 {
		return errors.New("pause_limits.acknowledge and pause_limits.cleanup must be positive")
	}
	return nil
}

// postCommand issues a command to a task that is not stopped or failed,
// and returns it.
func (s *Server) postCommand(w http.ResponseWriter, r *http.Request) {
	task, err := protocol.ParseTaskID(r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var request commandRequest
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxOwnerRequestBytes), &request); err != nil {
		http.Error(w, "decode command: "+err.Error(), http.StatusBadRequest)
		return
	}
	payload, err := commandPayload(request.Kind, request.Payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	command, err := s.store.issueCommand(r.Context(), task, request.Kind, payload)
	if errors.Is(err, errUnknownTask) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if errors.Is(err, errTaskEnded) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, command)
}

// commandPayload checks raw against what kind carries and returns it
// re-encoded, or nil for a kind that carries none.
func commandPayload(kind protocol.CommandKind, raw json.RawMessage) (json.RawMessage, error) {
	var payload any
	switch kind {
	case protocol.CommandPause, protocol.CommandResume, protocol.CommandInterrupt, protocol.CommandStop:
		if len(raw) != 0 && string(raw) != "null" {
			return nil, fmt.Errorf("%s takes no payload", kind)
		}
		return nil, nil
	case protocol.CommandPrompt:
		var prompt protocol.Prompt
		if err := decodeStrict(bytes.NewReader(raw), &prompt); err != nil {
			return nil, fmt.Errorf("decode prompt: %w", err)
		}
		if prompt.Text == "" {
			return nil, errors.New("prompt text is required")
		}
		if prompt.From != nil {
			return nil, errors.New("prompt from is set only by the server, for a message it delivers")
		}
		payload = prompt
	case protocol.CommandAnswerPermission:
		var answer protocol.AnswerPermission
		if err := decodeStrict(bytes.NewReader(raw), &answer); err != nil {
			return nil, fmt.Errorf("decode answer_permission: %w", err)
		}
		if answer.RequestID == "" {
			return nil, errors.New("answer_permission request_id is required")
		}
		payload = answer
	case protocol.CommandStartTask:
		return nil, errors.New("start a task with POST /v1/tasks")
	default:
		return nil, fmt.Errorf("unknown command kind %q", kind)
	}
	return json.Marshal(payload)
}

// getEvents returns a task's stored events after the seq in the "after"
// query parameter (0 when absent), in seq order.
func (s *Server) getEvents(w http.ResponseWriter, r *http.Request) {
	task, err := protocol.ParseTaskID(r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var after uint64
	if raw := r.URL.Query().Get("after"); raw != "" {
		if after, err = strconv.ParseUint(raw, 10, 63); err != nil {
			http.Error(w, "after is not a seq", http.StatusBadRequest)
			return
		}
	}
	events, err := s.store.eventsAfter(r.Context(), task, after)
	if errors.Is(err, errUnknownTask) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// decodeStrict decodes one JSON value and refuses fields the target does
// not have, so that a misspelt field is reported rather than dropped.
func decodeStrict(r io.Reader, into any) error {
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

// getTasks lists every task's summary, oldest first.
func (s *Server) getTasks(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.tasks(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tasks)
}

// getTask returns one task's summary and what it was started with.
func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	task, err := protocol.ParseTaskID(r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	detail, err := s.store.task(r.Context(), task)
	if errors.Is(err, errUnknownTask) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}
