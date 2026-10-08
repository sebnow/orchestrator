package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// The pause limits the new-task form starts with.
const (
	defaultPauseAcknowledge = "1m"
	defaultPauseCleanup     = "5m"
)

// routeGUI serves the owner's GUI: server-rendered pages whose forms POST
// to the routes below and work with JavaScript off, and which htmx keeps
// current when it is on.
func (s *Server) routeGUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.getDashboard)
	mux.HandleFunc("POST /tasks", s.postTaskForm)
	mux.HandleFunc("GET /tasks/{task}", s.getTaskPage)
	mux.HandleFunc("POST /tasks/{task}/commands", s.postCommandForm)
	mux.HandleFunc("GET /tasks/{task}/raw", s.getRawPage)
	mux.HandleFunc("GET /tasks/{task}/stream", s.streamTask)
	mux.HandleFunc("GET /tasks/{task}/updates", s.getTaskUpdates)
}

// fromHTMX reports whether htmx made the request, in which case the
// response is a fragment rather than a page or a redirect.
func fromHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

func (s *Server) writeHTML(w http.ResponseWriter, status int, node html.Node) {
	var buf bytes.Buffer
	if err := html.Render(&buf, node); err != nil {
		s.internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// redirect sends a browser without JavaScript to the page showing the
// outcome of its POST.
func redirect(w http.ResponseWriter, r *http.Request, url string) {
	http.Redirect(w, r, url, http.StatusSeeOther)
}

func guiTask(summary taskSummary, prompt string) component.Task {
	return component.Task{
		ID:             string(summary.ID),
		State:          string(summary.State),
		Daemon:         string(summary.DaemonID),
		Model:          summary.Model,
		Prompt:         prompt,
		CreatedAt:      summary.CreatedAt,
		LastActivityAt: summary.LastActivityAt,
		CostUSD:        summary.CostUSD,
	}
}

func (s *Server) getDashboard(w http.ResponseWriter, r *http.Request) {
	s.writeDashboard(w, r, http.StatusOK, component.NewTask{Acknowledge: defaultPauseAcknowledge, Cleanup: defaultPauseCleanup}, "")
}

// writeDashboard writes the dashboard with input in the new-task form,
// and problem, when set, saying why it was refused.
func (s *Server) writeDashboard(w http.ResponseWriter, r *http.Request, status int, input component.NewTask, problem string) {
	lists, daemons, err := s.dashboardLists(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeHTML(w, status, component.Page("Tasks",
		component.Refreshing(component.RegionDashboard, "/", lists),
		component.Section("New task", component.RegionOf(component.RegionNewTask,
			component.NewTaskForm(input, daemons, s.defaultModel, problem, ""))),
	))
}

// dashboardLists renders the tasks needing attention, every task, newest
// first, and every daemon. It also returns the daemons' ids.
func (s *Server) dashboardLists(ctx context.Context) (html.Node, []string, error) {
	summaries, err := s.store.tasks(ctx)
	if err != nil {
		return nil, nil, err
	}
	prompts, err := s.store.taskPrompts(ctx)
	if err != nil {
		return nil, nil, err
	}
	daemons, err := s.store.daemons(ctx)
	if err != nil {
		return nil, nil, err
	}
	var attention []component.Attention
	taskRows := make([]html.Node, 0, len(summaries))
	for _, summary := range slices.Backward(summaries) {
		task := guiTask(summary, prompts[summary.ID])
		taskRows = append(taskRows, component.TaskRow(task))
		reason, err := s.attentionReason(ctx, summary)
		if err != nil {
			return nil, nil, err
		}
		if reason != "" {
			attention = append(attention, component.Attention{Task: task, Reason: reason})
		}
	}
	ids := make([]string, len(daemons))
	daemonRows := make([]html.Node, len(daemons))
	for idx, daemon := range daemons {
		ids[idx] = string(daemon.ID)
		row := component.Daemon{ID: string(daemon.ID), LastSeen: daemon.LastSeen, Quota: daemon.Quota, QuotaAt: daemon.QuotaAt}
		if daemon.Harness != nil {
			row.Harness = daemon.Harness.Name + " " + daemon.Harness.Version
		}
		daemonRows[idx] = component.DaemonRow(row)
	}
	return html.Fragment(
		component.Section("Needs attention", component.AttentionList(attention)),
		component.Section("Tasks", component.Table(component.TaskColumns, "No tasks yet.", taskRows...)),
		component.Section("Daemons", component.Table(component.DaemonColumns, "No daemon has connected yet.", daemonRows...)),
	), ids, nil
}

// attentionReason says why a task waits for the owner, or returns "" when
// it does not: it awaits permission, is paused, or failed.
func (s *Server) attentionReason(ctx context.Context, summary taskSummary) (string, error) {
	if summary.State != TaskAwaitingPermission && summary.State != TaskPaused && summary.State != TaskFailed {
		return "", nil
	}
	entries, err := s.Transcript(ctx, summary.ID)
	if err != nil {
		return "", err
	}
	switch summary.State {
	case TaskAwaitingPermission:
		if pending := pendingPermissions(entries); len(pending) > 0 {
			return "asks to run " + pending[0].Tool, nil
		}
		return "awaits permission", nil
	case TaskPaused:
		for _, entry := range slices.Backward(entries) {
			if ack, ok := entry.Body.(transcript.PauseAcknowledged); ok && ack.Note != "" {
				return "paused: " + ack.Note, nil
			}
		}
		return "paused", nil
	default:
		for _, entry := range slices.Backward(entries) {
			if exit, ok := entry.Body.(transcript.HarnessExited); ok {
				if exit.Error != "" {
					return "failed: " + exit.Error, nil
				}
				return "harness exited with code " + strconv.Itoa(exit.ExitCode), nil
			}
		}
		return "failed", nil
	}
}

// pendingPermissions returns the permission requests in entries that no
// answer in entries names, in the order they were made. Answers are
// matched wherever they fall, because a request and its answer are
// stamped by different clocks.
func pendingPermissions(entries []transcript.Entry) []transcript.PermissionRequested {
	answered := make(map[string]bool)
	for _, entry := range entries {
		if answer, ok := entry.Body.(transcript.PermissionAnswered); ok {
			answered[answer.RequestID] = true
		}
	}
	var pending []transcript.PermissionRequested
	for _, entry := range entries {
		if request, ok := entry.Body.(transcript.PermissionRequested); ok && !answered[request.RequestID] {
			pending = append(pending, request)
		}
	}
	return pending
}

// postTaskForm starts a task from the new-task form.
func (s *Server) postTaskForm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxOwnerRequestBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "read form: "+err.Error(), http.StatusBadRequest)
		return
	}
	input := component.NewTask{
		Prompt:      r.PostForm.Get("prompt"),
		Repo:        strings.TrimSpace(r.PostForm.Get("repo")),
		Ref:         strings.TrimSpace(r.PostForm.Get("ref")),
		Model:       strings.TrimSpace(r.PostForm.Get("model")),
		Daemon:      r.PostForm.Get("daemon"),
		Acknowledge: strings.TrimSpace(r.PostForm.Get("acknowledge")),
		Cleanup:     strings.TrimSpace(r.PostForm.Get("cleanup")),
	}
	command, problem, err := s.startTaskFromForm(r.Context(), input)
	if err != nil {
		s.internalError(w, err)
		return
	}
	switch {
	case problem != "" && fromHTMX(r):
		_, daemons, err := s.dashboardLists(r.Context())
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.writeHTML(w, http.StatusUnprocessableEntity, component.OutOfBand(component.RegionNewTask,
			component.NewTaskForm(input, daemons, s.defaultModel, problem, "")))
	case problem != "":
		s.writeDashboard(w, r, http.StatusUnprocessableEntity, input, problem)
	case fromHTMX(r):
		lists, daemons, err := s.dashboardLists(r.Context())
		if err != nil {
			s.internalError(w, err)
			return
		}
		fresh := component.NewTask{Daemon: input.Daemon, Acknowledge: input.Acknowledge, Cleanup: input.Cleanup}
		s.writeHTML(w, http.StatusOK, html.Fragment(
			component.OutOfBand(component.RegionNewTask,
				component.NewTaskForm(fresh, daemons, s.defaultModel, "", string(command.TaskID))),
			component.OutOfBand(component.RegionDashboard, lists),
		))
	default:
		redirect(w, r, "/tasks/"+string(command.TaskID))
	}
}

// startTaskFromForm starts the task input describes. A problem with the
// input is returned as text for the owner, with no error.
func (s *Server) startTaskFromForm(ctx context.Context, input component.NewTask) (protocol.Command, string, error) {
	daemon, err := protocol.ParseDaemonID(input.Daemon)
	if err != nil {
		return protocol.Command{}, "Choose a daemon to run the task on.", nil
	}
	acknowledge, err := time.ParseDuration(input.Acknowledge)
	if err != nil {
		return protocol.Command{}, "The acknowledge limit is not a duration such as 1m or 90s.", nil
	}
	cleanup, err := time.ParseDuration(input.Cleanup)
	if err != nil {
		return protocol.Command{}, "The cleanup limit is not a duration such as 5m.", nil
	}
	start := protocol.StartTask{
		Prompt:      input.Prompt,
		Model:       input.Model,
		PauseLimits: protocol.PauseLimits{Acknowledge: acknowledge, Cleanup: cleanup},
	}
	if input.Repo != "" || input.Ref != "" {
		start.Workspace = &protocol.Workspace{Repo: input.Repo, Ref: input.Ref}
	}
	if strings.TrimSpace(start.Prompt) == "" {
		return protocol.Command{}, "Write a prompt for the task.", nil
	}
	if err := validateStart(start); err != nil {
		return protocol.Command{}, "The task was not started: " + err.Error() + ".", nil
	}
	command, err := s.startTask(ctx, daemon, start)
	if errors.Is(err, errUnknownDaemon) {
		return protocol.Command{}, "Daemon " + input.Daemon + " has not connected yet.", nil
	}
	return command, "", err
}
