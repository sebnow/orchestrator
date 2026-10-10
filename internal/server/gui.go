package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// routeGUI serves the owner's GUI: server-rendered pages whose forms POST
// to the routes below and work with JavaScript off, and which htmx keeps
// current when it is on.
func (s *Server) routeGUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /{$}", s.getDashboard)
	mux.HandleFunc("POST /tasks", s.postTaskForm)
	mux.HandleFunc("GET /tasks/{task}", s.getTaskPage)
	mux.HandleFunc("POST /tasks/{task}/commands", s.postCommandForm)
	mux.HandleFunc("POST /tasks/{task}/dismiss", s.postDismissForm)
	mux.HandleFunc("POST /tasks/{task}/continue", s.postContinueForm)
	mux.HandleFunc("GET /tasks/{task}/raw", s.getRawPage)
	mux.HandleFunc("GET /tasks/{task}/stream", s.streamTask)
	mux.HandleFunc("GET /tasks/{task}/updates", s.getTaskUpdates)
	s.routeAgentsGUI(mux)
	s.routeProjectsGUI(mux)
	mux.HandleFunc("GET /daemons/{daemon}", s.getDaemonPage)
	mux.HandleFunc("POST /daemons/{daemon}/labels", s.postLabelsForm)
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

// modelText is the task's model, or, until placement chooses it, the
// models it is chosen from.
func modelText(summary taskSummary) string {
	if summary.Model == "" && len(summary.Models) > 0 {
		return alternatives(summary.Models) + ", once placed"
	}
	return summary.Model
}

func guiTask(summary taskSummary, prompt string) component.Task {
	task := component.Task{
		ID:             string(summary.ID),
		Agent:          summary.Agent,
		Project:        summary.Project,
		Purpose:        summary.Purpose,
		Requires:       summary.Requires,
		State:          string(summary.State),
		Daemon:         string(summary.DaemonID),
		Model:          modelText(summary),
		Prompt:         prompt,
		CreatedAt:      summary.CreatedAt,
		LastActivityAt: summary.LastActivityAt,
		CostUSD:        summary.CostUSD,
		Priority:       string(summary.Priority),
		Filler:         summary.Filler,
	}
	if summary.ParentID != nil {
		task.Parent = string(*summary.ParentID)
	}
	if summary.Queue != nil {
		task.Queue = &component.QueuePlace{Position: summary.Queue.Position, Reason: summary.Queue.Reason}
	}
	if summary.DismissedAt != nil {
		task.DismissedAt = *summary.DismissedAt
	}
	if summary.Branch != nil {
		pushed := transcript.BranchPushed(*summary.Branch)
		task.Branch = &pushed
	}
	return task
}

// showsDismissed reports whether the request asks for the dashboard to
// list dismissed tasks. A form htmx posts from the dashboard carries the
// dashboard's address in HX-Current-URL.
func showsDismissed(r *http.Request) bool {
	query := r.URL.Query()
	if fromHTMX(r) && !query.Has(component.DismissedParam) {
		if current, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil {
			query = current.Query()
		}
	}
	return query.Get(component.DismissedParam) == component.DismissedShown
}

func (s *Server) getDashboard(w http.ResponseWriter, r *http.Request) {
	s.writeDashboard(w, r, http.StatusOK, component.NewTask{}, "")
}

// writeDashboard writes the dashboard with input in the new-task form,
// and problem, when set, saying why it was refused.
func (s *Server) writeDashboard(w http.ResponseWriter, r *http.Request, status int, input component.NewTask, problem string) {
	shown := showsDismissed(r)
	lists, daemons, err := s.dashboardLists(r.Context(), shown)
	if err != nil {
		s.internalError(w, err)
		return
	}
	form, err := s.newTaskForm(r.Context(), input, daemons, problem, "")
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeHTML(w, status, component.Page("Tasks",
		component.Refreshing(component.RegionDashboard, component.DashboardURL(shown), lists),
		component.Section("New task", component.RegionOf(component.RegionNewTask, form)),
	))
}

// newTaskForm is the new-task form with input, offering daemons, every
// agent and every project.
func (s *Server) newTaskForm(ctx context.Context, input component.NewTask, daemons []string, problem, created string) (html.Node, error) {
	choices, err := s.taskChoices(ctx, daemons)
	if err != nil {
		return nil, err
	}
	return component.NewTaskForm(input, choices, problem, created), nil
}

// taskChoices are what the new-task forms offer: daemons, every agent and
// every project.
func (s *Server) taskChoices(ctx context.Context, daemons []string) (component.TaskChoices, error) {
	agents, err := s.store.agents(ctx)
	if err != nil {
		return component.TaskChoices{}, err
	}
	projects, err := s.store.projects(ctx)
	if err != nil {
		return component.TaskChoices{}, err
	}
	choices := component.TaskChoices{
		Daemons: daemons, Agents: make([]string, len(agents)), Projects: make([]component.Option, len(projects)),
		DefaultModel: s.defaultModel, DefaultAcknowledge: defaultPauseLimits.Acknowledge.String(), DefaultCleanup: defaultPauseLimits.Cleanup.String(),
	}
	for idx, a := range agents {
		choices.Agents[idx] = a.Name
	}
	for idx, p := range projects {
		choices.Projects[idx] = component.Option{Value: p.ID, Label: p.Name}
	}
	return choices, nil
}

// dashboardLists renders the tasks needing attention, every task, newest
// first, the account's quota reading, and every daemon. It also returns
// the daemons' ids. Dismissed tasks need no attention, and the task list
// leaves them out unless showDismissed is set.
func (s *Server) dashboardLists(ctx context.Context, showDismissed bool) (html.Node, []string, error) {
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
	reading, err := s.store.reading(ctx)
	if err != nil {
		return nil, nil, err
	}
	var attention []component.Attention
	taskRows := make([]html.Node, 0, len(summaries))
	dismissed := 0
	for _, summary := range slices.Backward(summaries) {
		task := guiTask(summary, prompts[summary.ID])
		if summary.DismissedAt != nil {
			dismissed++
			if showDismissed {
				taskRows = append(taskRows, component.TaskRow(task))
			}
			continue
		}
		taskRows = append(taskRows, component.TaskRow(task))
		reason, err := s.attentionReason(ctx, summary)
		if err != nil {
			return nil, nil, err
		}
		if reason != "" {
			attention = append(attention, component.Attention{Task: task, Reason: reason, Dismissable: summary.State.Ended()})
		}
	}
	connected := s.connectedDaemons()
	ids := make([]string, len(daemons))
	daemonRows := make([]html.Node, len(daemons))
	for idx, daemon := range daemons {
		ids[idx] = string(daemon.ID)
		row := component.Daemon{
			ID: string(daemon.ID), LastSeen: daemon.LastSeen, Quota: daemon.Quota, QuotaAt: daemon.QuotaAt,
			Connected: slices.Contains(connected, daemon.ID), Slots: s.sched.policy.capacity(daemon.Facts, daemon.Labels), InUse: daemon.InUse,
			Labels: Merge(daemon.Facts, daemon.Labels),
		}
		if daemon.LostAt != nil {
			row.LostSince = *daemon.LostAt
		}
		row.Harness = daemonHarness(daemon)
		daemonRows[idx] = component.DaemonRow(row)
	}
	return html.Fragment(
		component.Section("Needs attention", component.AttentionList(attention, component.DashboardURL(showDismissed))),
		component.Section("Tasks", component.Table(component.TaskColumns, "No tasks yet.", taskRows...),
			component.DismissedToggle(dismissed, showDismissed)),
		component.Section("Budget", s.budget(reading)),
		component.Section("Daemons", component.Table(component.DaemonColumns, "No daemon has connected yet.", daemonRows...)),
	), ids, nil
}

// daemonHarness names daemon's harness and its version: those of the
// latest event the daemon sent, or before it has sent any, those it
// reported among its facts when it connected; "" when neither says.
func daemonHarness(daemon daemonSummary) string {
	if daemon.Harness != nil {
		return daemon.Harness.Name + " " + daemon.Harness.Version
	}
	name, version := daemon.Facts[protocol.FactHarness], daemon.Facts[protocol.FactHarnessVersion]
	if name == "" || version == "" {
		return name
	}
	return name + " " + version
}

// budget shows the account's quota reading as the scheduler uses it.
func (s *Server) budget(reading *quotaReading) html.Node {
	policy := s.sched.policy
	if reading == nil {
		return component.Budget(nil, time.Time{}, 0, policy.FillerThreshold, policy.LowThreshold)
	}
	return component.Budget(&reading.QuotaObserved, reading.At, s.sched.now().Sub(reading.At), policy.FillerThreshold, policy.LowThreshold)
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
		// Only the latest process can have paused the task.
	latest:
		for _, entry := range slices.Backward(entries) {
			switch body := entry.Body.(type) {
			case transcript.PauseAcknowledged:
				if body.Note != "" {
					return "paused: " + body.Note, nil
				}
				break latest
			case transcript.HarnessExited:
				switch body.CutShortBy {
				case "":
				case "interrupted":
					return "paused: interrupted by the owner", nil
				default:
					return "paused: the daemon " + body.CutShortBy + " during its turn", nil
				}
			case transcript.HarnessStarted:
				break latest
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
// stamped by different clocks. A request before the owner's steering
// prompt is not pending: steering interrupted the turn that made it.
func pendingPermissions(
	entries []transcript.Entry) []transcript.PermissionRequested {
	answered := make(map[string]bool)
	for _, entry := range entries {
		if answer, ok := entry.Body.(transcript.PermissionAnswered); ok {
			answered[answer.RequestID] = true
		}
	}
	var pending []transcript.PermissionRequested
	for _, entry := range entries {
		switch body := entry.Body.(type) {
		case transcript.PermissionRequested:
			if !answered[body.RequestID] {
				pending = append(pending, body)
			}
		case transcript.OwnerPrompt:
			// Steering interrupts the turn, and with it every request the
			// turn made.
			if body.Steer {
				pending = nil
			}
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
		Agent:       r.PostForm.Get("agent"),
		Project:     r.PostForm.Get("project"),
		Purpose:     r.PostForm.Get("purpose"),
		Requires:    strings.TrimSpace(r.PostForm.Get("requires")),
		Prompt:      r.PostForm.Get("prompt"),
		Repo:        strings.TrimSpace(r.PostForm.Get("repo")),
		Ref:         strings.TrimSpace(r.PostForm.Get("ref")),
		Model:       strings.TrimSpace(r.PostForm.Get("model")),
		Daemon:      r.PostForm.Get("daemon"),
		Acknowledge: strings.TrimSpace(r.PostForm.Get("acknowledge")),
		Cleanup:     strings.TrimSpace(r.PostForm.Get("cleanup")),
		Priority:    r.PostForm.Get("priority"),
		Filler:      r.PostForm.Get("filler"),
	}
	task, problem, err := s.startTaskFromForm(r.Context(), input)
	if err != nil {
		s.internalError(w, err)
		return
	}
	switch {
	case problem != "" && fromHTMX(r):
		_, daemons, err := s.dashboardLists(r.Context(), showsDismissed(r))
		if err != nil {
			s.internalError(w, err)
			return
		}
		form, err := s.newTaskForm(r.Context(), input, daemons, problem, "")
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.writeHTML(w, http.StatusUnprocessableEntity, component.OutOfBand(component.RegionNewTask, form))
	case problem != "":
		s.writeDashboard(w, r, http.StatusUnprocessableEntity, input, problem)
	case fromHTMX(r):
		lists, daemons, err := s.dashboardLists(r.Context(), showsDismissed(r))
		if err != nil {
			s.internalError(w, err)
			return
		}
		fresh := component.NewTask{Agent: input.Agent, Project: input.Project, Daemon: input.Daemon, Acknowledge: input.Acknowledge, Cleanup: input.Cleanup, Priority: input.Priority, Filler: input.Filler}
		form, err := s.newTaskForm(r.Context(), fresh, daemons, "", string(task))
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.writeHTML(w, http.StatusOK, html.Fragment(
			component.OutOfBand(component.RegionNewTask, form),
			component.OutOfBand(component.RegionDashboard, lists),
		))
	default:
		redirect(w, r, "/tasks/"+string(task))
	}
}

// startTaskFromForm queues the start of the task input describes, and
// returns its id. A problem with the input is returned as text for the
// owner, with no error.
func (s *Server) startTaskFromForm(ctx context.Context, input component.NewTask) (protocol.TaskID, string, error) {
	var daemon protocol.DaemonID
	if input.Daemon != "" {
		var err error
		if daemon, err = protocol.ParseDaemonID(input.Daemon); err != nil {
			return "", "Choose a daemon to run the task on, or any.", nil
		}
	}
	var priority Priority
	if input.Priority != "" {
		var err error
		if priority, err = ParsePriority(input.Priority); err != nil {
			return "", "Choose low, normal or high priority.", nil
		}
	}
	// A task naming no agent in a project is started as the project's
	// default agent, if it has one.
	agent := input.Agent
	if input.Project != "" && agent == "" {
		p, err := s.store.project(ctx, input.Project)
		if errors.Is(err, errUnknownProject) {
			return "", "There is no project " + input.Project + ".", nil
		}
		if err != nil {
			return "", "", err
		}
		agent = p.DefaultAgent
	}
	// Blank pause limits are the agent's, or else the defaults.
	var limits protocol.PauseLimits
	switch {
	case input.Acknowledge == "" && input.Cleanup == "" && agent != "":
	case input.Acknowledge == "" && input.Cleanup == "":
		limits = defaultPauseLimits
	case input.Acknowledge == "" || input.Cleanup == "":
		return "", "Give both pause limits, or neither for the agent's or the defaults.", nil
	default:
		var err error
		if limits.Acknowledge, err = time.ParseDuration(input.Acknowledge); err != nil {
			return "", "The acknowledge limit is not a duration such as 1m or 90s.", nil
		}
		if limits.Cleanup, err = time.ParseDuration(input.Cleanup); err != nil {
			return "", "The cleanup limit is not a duration such as 5m.", nil
		}
	}
	start := protocol.StartTask{
		Prompt:      input.Prompt,
		Model:       input.Model,
		PauseLimits: limits,
	}
	if input.Repo != "" || input.Ref != "" {
		start.Workspace = &protocol.Workspace{Repo: input.Repo, Ref: input.Ref}
	}
	if strings.TrimSpace(start.Prompt) == "" {
		return "", "Write a prompt for the task.", nil
	}
	// An unticked box leaves the agent's choice.
	var filler *bool
	if input.Filler != "" {
		on := true
		filler = &on
	}
	// Blank requires are the agent's.
	var requires *Labels
	if input.Requires != "" {
		parsed, err := ParseLabels(input.Requires)
		if err != nil {
			return "", "Requires: " + strings.TrimPrefix(err.Error(), errInvalidLabels.Error()+": ") + ".", nil
		}
		requires = &parsed
	}
	turn, err := s.startTask(ctx, taskRequest{Daemon: daemon, Agent: input.Agent, Project: input.Project, Purpose: input.Purpose, Requires: requires, Priority: priority, Filler: filler, Start: start})
	if errors.Is(err, errInvalidTask) {
		return "", "The task was not started: " + strings.TrimPrefix(err.Error(), errInvalidTask.Error()+": ") + ".", nil
	}
	if errors.Is(err, errUnknownAgent) {
		return "", "There is no agent " + agent + ".", nil
	}
	if errors.Is(err, errUnknownProject) {
		return "", "There is no project " + input.Project + ".", nil
	}
	if errors.Is(err, errUnknownDaemon) {
		return "", "Daemon " + input.Daemon + " has not connected yet.", nil
	}
	if errors.Is(err, errNoDaemon) {
		return "", "No daemon has connected yet.", nil
	}
	return turn.TaskID, "", err
}
