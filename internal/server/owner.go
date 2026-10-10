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
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// maxOwnerRequestBytes bounds an owner's request body, which carries at
// most a prompt and a system prompt.
const maxOwnerRequestBytes = 1 << 20

// createTaskRequest is the body of POST /v1/tasks: the start_task payload,
// the daemon to run it on, or none for any, how it is scheduled, the
// agent it is started as, if any, and the project it belongs to, if any.
// A field left out takes the agent's value
// (docs/adr/2026-10-09-agents-and-placement.md), and the agent left out
// the project's default (docs/adr/2026-10-10-projects-and-lineage.md).
type createTaskRequest struct {
	DaemonID protocol.DaemonID `json:"daemon_id"`
	Agent    string            `json:"agent"`
	Project  string            `json:"project"`
	Purpose  string            `json:"purpose"`
	Requires *Labels           `json:"requires"`
	Priority string            `json:"priority"`
	Filler   *bool             `json:"filler"`
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
	var priority Priority
	if request.Priority != "" {
		var err error
		if priority, err = ParsePriority(request.Priority); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	turn, err := s.startTask(r.Context(), taskRequest{
		Daemon: daemon, Agent: request.Agent, Project: request.Project, Purpose: request.Purpose, Requires: request.Requires, Priority: priority, Filler: request.Filler, Start: request.StartTask,
	})
	if errors.Is(err, errInvalidTask) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if errors.Is(err, errUnknownAgent) || errors.Is(err, errUnknownProject) {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
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

// taskRequest is a task the owner asks for. An empty Daemon is any
// connected daemon. Agent names the agent the task is started as, if
// any, whose values the task takes where the request leaves them out:
// an empty Priority, a nil Filler, a nil Requires, an empty
// Start.Model, a nil Start.Tools and zero Start.PauseLimits. Project is
// the id of the project the task belongs to, if any, whose default agent
// an empty Agent takes and whose repository the task works in. Purpose,
// when set, says why the task exists, and is put at the top of its
// prompt.
type taskRequest struct {
	Daemon   protocol.DaemonID
	Agent    string
	Project  string
	Purpose  string
	Requires *Labels
	Priority Priority
	Filler   *bool
	Start    protocol.StartTask
}

// defaultPauseLimits are the pause limits of a task started as an agent
// that sets none, and the GUI's for a task it leaves them out of.
var defaultPauseLimits = protocol.PauseLimits{Acknowledge: time.Minute, Cleanup: 5 * time.Minute}

// errInvalidTask reports a task request that is refused as it stands.
var errInvalidTask = errors.New("invalid task")

// startTask creates the task request asks for under a new id, and
// returns its queued start. What the request and its agent leave out is
// the defaults: normal priority, not filler, the server's model, and
// both gateway tools. The system prompt is composed from the server's
// instructions, the agent's system prompt, the project's instructions and
// the request's.
func (s *Server) startTask(ctx context.Context, request taskRequest) (queuedTurn, error) {
	start := request.Start
	var instructions string
	if request.Project != "" {
		p, err := s.store.project(ctx, request.Project)
		if err != nil {
			return queuedTurn{}, err
		}
		instructions = p.Instructions
		if request.Agent == "" {
			request.Agent = p.DefaultAgent
		}
		if start.Workspace, err = projectWorkspace(p, start.Workspace); err != nil {
			return queuedTurn{}, err
		}
	}
	priority, filler := request.Priority, false
	requires := Labels{}
	var agentPrompt string
	if request.Agent != "" {
		a, err := s.store.agent(ctx, request.Agent)
		if err != nil {
			return queuedTurn{}, err
		}
		applyAgent(&start, a)
		if priority == "" {
			priority = a.Priority
		}
		filler, agentPrompt, requires = a.Filler, a.SystemPrompt, a.Requires
	}
	if request.Requires != nil {
		requires = *request.Requires
	}
	if err := requires.Validate(); err != nil {
		return queuedTurn{}, fmt.Errorf("%w: requires: %v", errInvalidTask, err)
	}
	if request.Filler != nil {
		filler = *request.Filler
	}
	if priority == "" {
		priority = PriorityNormal
	}
	if start.Model == "" {
		start.Model = s.defaultModel
	}
	if err := validateStart(start); err != nil {
		return queuedTurn{}, fmt.Errorf("%w: %v", errInvalidTask, err)
	}
	agents, err := s.store.agents(ctx)
	if err != nil {
		return queuedTurn{}, err
	}
	start.SystemPrompt = systemPrompt(promptParts{Tools: start.Tools, Agents: agents, Agent: agentPrompt, Project: instructions, Task: start.SystemPrompt})
	purpose := oneLine(request.Purpose)
	start.Prompt = withPurpose(purpose, start.Prompt)
	placed := placementBound
	if request.Daemon == "" {
		placed = placementAny
	}
	return s.store.createTask(ctx, newTask{
		// rand.Text uses only letters and digits, so the id is always valid.
		ID:        protocol.TaskID(rand.Text()),
		Daemon:    request.Daemon,
		Placement: placed,
		Agent:     request.Agent,
		Project:   request.Project,
		Purpose:   purpose,
		Requires:  requires,
		Priority:  priority,
		Filler:    filler,
		Start:     start,
		Origin:    originOwner,
	})
}

// projectWorkspace is the workspace of a task in project p that asks for
// asked: the project's repository, or when it has none, asked. A task
// asking for another repository than its project's is refused with
// errInvalidTask.
func projectWorkspace(p Project, asked *protocol.Workspace) (*protocol.Workspace, error) {
	own := p.Workspace()
	switch {
	case own == nil:
		return asked, nil
	case asked != nil && *asked != *own:
		return nil, fmt.Errorf("%w: a task in project %s works in the project's repository, %s at %s; leave the repository out", errInvalidTask, p.Name, own.Repo, own.Ref)
	}
	return own, nil
}

// applyAgent fills in what start leaves out from agent a: its model, its
// tools, and its pause limits, or the default ones when a has none.
func applyAgent(start *protocol.StartTask, a Agent) {
	if start.Model == "" {
		start.Model = a.Model
	}
	if start.Tools == nil {
		start.Tools = a.Tools
	}
	if start.PauseLimits == (protocol.PauseLimits{}) {
		start.PauseLimits = defaultPauseLimits
		if a.PauseLimits != nil {
			start.PauseLimits = *a.PauseLimits
		}
	}
}

// spawnable lists agents for a task allowed tools, if those let it spawn.
func spawnable(tools []string, agents []Agent) string {
	if tools != nil && !slices.Contains(tools, protocol.ToolSpawnTask) {
		return ""
	}
	return agentsPrompt(agents)
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
