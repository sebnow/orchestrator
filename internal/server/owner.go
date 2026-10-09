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
	"slices"
	"strconv"
	"strings"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// maxOwnerRequestBytes bounds an owner's request body, which carries at
// most a prompt and a system prompt.
const maxOwnerRequestBytes = 1 << 20

// createTaskRequest is the body of POST /v1/tasks: the start_task payload,
// the daemon to run it on, or none for any, and how it is scheduled.
type createTaskRequest struct {
	DaemonID protocol.DaemonID `json:"daemon_id"`
	Priority string            `json:"priority"`
	Filler   bool              `json:"filler"`
	protocol.StartTask
}

// commandRequest is the body of POST /v1/tasks/{task}/commands.
type commandRequest struct {
	Kind    protocol.CommandKind `json:"kind"`
	Payload json.RawMessage      `json:"payload"`
}

// postTask creates a task on the named daemon, or on any, and queues its
// start, and returns the queued turn.
func (s *Server) postTask(w http.ResponseWriter, r *http.Request) {
	var request createTaskRequest
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxOwnerRequestBytes), &request); err != nil {
		http.Error(w, "decode task: "+err.Error(), http.StatusBadRequest)
		return
	}
	var daemon protocol.DaemonID
	if request.DaemonID != "" {
		var err error
		if daemon, err = protocol.ParseDaemonID(string(request.DaemonID)); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	priority, err := ParsePriority(request.Priority)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateStart(request.StartTask); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	turn, err := s.startTask(r.Context(), daemon, priority, request.Filler, request.StartTask)
	if errors.Is(err, errUnknownDaemon) {
		http.Error(w, err.Error()+": it has not connected yet", http.StatusUnprocessableEntity)
		return
	}
	if errors.Is(err, errNoDaemon) {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, turn)
}

// startTask creates a task under a new id, on daemon or, when that is
// empty, on any connected daemon, with the default model when start names
// none and the composed system prompt, and returns its queued start.
func (s *Server) startTask(ctx context.Context, daemon protocol.DaemonID, priority Priority, filler bool, start protocol.StartTask) (queuedTurn, error) {
	if start.Model == "" {
		start.Model = s.defaultModel
	}
	start.SystemPrompt = systemPrompt(nil, start.Tools, start.SystemPrompt)
	placed := placementBound
	if daemon == "" {
		placed = placementAny
	}
	return s.store.createTask(ctx, newTask{
		// rand.Text uses only letters and digits, so the id is always valid.
		ID:        protocol.TaskID(rand.Text()),
		Daemon:    daemon,
		Placement: placed,
		Priority:  priority,
		Filler:    filler,
		Start:     start,
		Origin:    originOwner,
	})
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
	return validateTools(start.Tools)
}

// validateTools checks that tools names gateway tools an agent may be
// allowed, each once.
func validateTools(tools []string) error {
	for idx, tool := range tools {
		if !slices.Contains(protocol.AgentTools, tool) {
			return fmt.Errorf("tools: %q is not one of %s", tool, strings.Join(protocol.AgentTools, ", "))
		}
		if slices.Contains(tools[:idx], tool) {
			return fmt.Errorf("tools: %q is named twice", tool)
		}
	}
	return nil
}

// postCommand acts on a task that is not stopped or failed. A prompt or
// a resume is queued as a turn, which it returns with 202. Any other
// command is issued at once and returned with 201, except a stop of a
// task that has not started, which ends it with nothing to send and gets
// 204.
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
	var status int
	var result any
	switch request.Kind {
	case protocol.CommandPrompt, protocol.CommandResume:
		status = http.StatusAccepted
		result, err = s.store.queueCommand(r.Context(), task, turnKind(request.Kind), payload)
	default:
		var command protocol.Command
		command, err = s.store.issueCommand(r.Context(), task, request.Kind, payload)
		status, result = http.StatusCreated, command
		if command.ID == 0 {
			status = http.StatusNoContent
		}
	}
	switch {
	case errors.Is(err, errUnknownTask):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errTaskEnded), errors.Is(err, errNotStarted):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		s.internalError(w, err)
	case status == http.StatusNoContent:
		w.WriteHeader(status)
	default:
		writeJSON(w, status, result)
	}
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

// postDismiss dismisses a stopped or failed task from the dashboard's
// lists and returns the task. A task that has not ended gets 409.
func (s *Server) postDismiss(w http.ResponseWriter, r *http.Request) {
	task, err := protocol.ParseTaskID(r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	detail, err := s.store.dismissTask(r.Context(), task)
	switch {
	case errors.Is(err, errUnknownTask):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errNotEnded):
		http.Error(w, err.Error()+": only a stopped or failed task can be dismissed", http.StatusConflict)
	case err != nil:
		s.internalError(w, err)
	default:
		writeJSON(w, http.StatusOK, detail)
	}
}
