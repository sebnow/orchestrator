package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// taskView is what a task page shows: the task and its transcript.
type taskView struct {
	detail  taskDetail
	entries []transcript.Entry
}

func (s *Server) readTaskView(ctx context.Context, task protocol.TaskID) (taskView, error) {
	detail, err := s.store.task(ctx, task)
	if err != nil {
		return taskView{}, err
	}
	entries, err := s.Transcript(ctx, task)
	if err != nil {
		return taskView{}, err
	}
	return taskView{detail: detail, entries: entries}, nil
}

// taskViewFromPath reads the task the request's path names, or writes the
// error and reports false.
func (s *Server) taskViewFromPath(w http.ResponseWriter, r *http.Request) (taskView, bool) {
	task, err := protocol.ParseTaskID(r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return taskView{}, false
	}
	view, err := s.readTaskView(r.Context(), task)
	if errors.Is(err, errUnknownTask) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return taskView{}, false
	}
	if err != nil {
		s.internalError(w, err)
		return taskView{}, false
	}
	return view, true
}

func (v taskView) id() string { return string(v.detail.ID) }

func (v taskView) task() component.Task { return guiTask(v.detail.taskSummary, v.detail.Start.Prompt) }

// header is the task's header with the controls its state offers.
func (v taskView) header() html.Node {
	state := v.detail.State
	return component.TaskHeader(v.task(), component.Controls(v.id(), component.ControlSet{
		Pause:     state == TaskRunning || state == TaskAwaitingPermission,
		Resume:    state == TaskPaused,
		Interrupt: !state.Ended(),
		Stop:      !state.Ended(),
	}))
}

// pending returns the permission requests waiting for an answer. The
// transcript decides, not the state, which reads pausing while a request
// waits; a task that has ended can no longer take an answer.
func (v taskView) pending() []transcript.PermissionRequested {
	if v.detail.State.Ended() {
		return nil
	}
	return pendingPermissions(v.entries)
}

func (v taskView) permission(problem string) html.Node {
	return component.PermissionPrompt(v.id(), v.pending(), problem)
}

func (v taskView) promptForm(text, problem string) html.Node {
	return component.PromptForm(v.id(), v.detail.State == TaskPausing, text, problem)
}

// refusal is a task form the server refused: what the owner entered and
// why it was refused, shown on the form it came from.
type refusal struct {
	kind    protocol.CommandKind
	text    string
	problem string
}

func (v taskView) page(refused refusal) html.Node {
	var promptText, promptProblem, permissionProblem string
	switch refused.kind {
	case protocol.CommandPrompt:
		promptText, promptProblem = refused.text, refused.problem
	case protocol.CommandAnswerPermission:
		permissionProblem = refused.problem
	}
	return component.Page("Task "+v.id(),
		component.RegionOf(component.RegionTaskHeader, v.header()),
		component.RegionOf(component.RegionPermission, v.permission(permissionProblem)),
		component.Section("Transcript", component.Transcript(v.entries)),
		component.Section("Follow up", component.RegionOf(component.RegionPrompt, v.promptForm(promptText, promptProblem))),
	)
}

func (s *Server) getTaskPage(w http.ResponseWriter, r *http.Request) {
	view, ok := s.taskViewFromPath(w, r)
	if !ok {
		return
	}
	s.writeHTML(w, http.StatusOK, view.page(refusal{}))
}

// postCommandForm issues the command a task page's form asks for.
func (s *Server) postCommandForm(w http.ResponseWriter, r *http.Request) {
	task, err := protocol.ParseTaskID(r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxOwnerRequestBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "read form: "+err.Error(), http.StatusBadRequest)
		return
	}
	refused := refusal{kind: protocol.CommandKind(r.PostForm.Get("kind"))}
	var payload any
	switch refused.kind {
	case protocol.CommandPause, protocol.CommandResume, protocol.CommandInterrupt, protocol.CommandStop:
	case protocol.CommandPrompt:
		refused.text = r.PostForm.Get("text")
		if strings.TrimSpace(refused.text) == "" {
			refused.problem = "Write a prompt first."
		}
		payload = protocol.Prompt{Text: refused.text}
	case protocol.CommandAnswerPermission:
		decision := r.PostForm.Get("decision")
		if decision != "allow" && decision != "deny" {
			refused.problem = "Choose allow or deny."
		}
		payload = protocol.AnswerPermission{
			RequestID: r.PostForm.Get("request_id"),
			Allow:     decision == "allow",
			Message:   strings.TrimSpace(r.PostForm.Get("message")),
		}
	default:
		http.Error(w, "unknown command kind "+string(refused.kind), http.StatusBadRequest)
		return
	}
	var checked json.RawMessage
	if refused.problem == "" {
		raw, err := json.Marshal(payload)
		if err != nil {
			s.internalError(w, err)
			return
		}
		if checked, err = commandPayload(refused.kind, raw); err != nil {
			refused.problem = err.Error()
		}
	}
	if refused.problem == "" {
		_, err = s.issueCommand(r.Context(), task, refused.kind, checked)
		if errors.Is(err, errUnknownTask) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if err != nil {
			s.internalError(w, err)
			return
		}
	}

	if !fromHTMX(r) && refused.problem == "" {
		redirect(w, r, "/tasks/"+string(task))
		return
	}
	view, ok := s.taskViewFromPath(w, r)
	if !ok {
		return
	}
	if !fromHTMX(r) {
		s.writeHTML(w, http.StatusUnprocessableEntity, view.page(refused))
		return
	}
	if refused.problem != "" {
		form := component.OutOfBand(component.RegionPermission, view.permission(refused.problem))
		if refused.kind == protocol.CommandPrompt {
			form = component.OutOfBand(component.RegionPrompt, view.promptForm(refused.text, refused.problem))
		}
		s.writeHTML(w, http.StatusUnprocessableEntity, form)
		return
	}
	var promptForm html.Node
	if refused.kind == protocol.CommandPrompt {
		promptForm = component.OutOfBand(component.RegionPrompt, view.promptForm("", ""))
	}
	s.writeHTML(w, http.StatusOK, html.Fragment(
		component.OutOfBand(component.RegionTaskHeader, view.header()),
		component.OutOfBand(component.RegionPermission, view.permission("")),
		component.OutOfBand(component.RegionPromptSubmit, component.PromptSubmit(view.detail.State == TaskPausing)),
		promptForm,
	))
}

// getRawPage shows the task's stored events as they were stored.
func (s *Server) getRawPage(w http.ResponseWriter, r *http.Request) {
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
	events, err := s.store.eventsAfter(r.Context(), task, 0)
	if err != nil {
		s.internalError(w, err)
		return
	}
	rows := make([]html.Node, len(events))
	for idx, event := range events {
		rows[idx] = component.EventRow(event)
	}
	s.writeHTML(w, http.StatusOK, component.Page("Events of task "+string(task),
		component.TaskHeader(guiTask(detail.taskSummary, detail.Start.Prompt), nil),
		component.Section("Stored events", component.Table(component.EventColumns, "No events stored yet.", rows...)),
	))
}
