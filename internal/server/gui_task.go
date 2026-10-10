package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// taskView is what a task page shows: the task and its transcript.
// Unrecognised entries are hidden unless showUnknown is set.
type taskView struct {
	detail taskDetail
	// projectName is the name of the task's project; empty for none.
	projectName string
	children    []childSummary
	tree        taskNode
	entries     []transcript.Entry
	showUnknown bool
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
	children, err := s.store.children(ctx, task)
	if err != nil {
		return taskView{}, err
	}
	tree, err := s.store.tree(ctx, task)
	if err != nil {
		return taskView{}, err
	}
	view := taskView{detail: detail, children: children, tree: tree, entries: entries}
	if detail.Project != "" {
		p, err := s.store.project(ctx, detail.Project)
		if err != nil {
			return taskView{}, err
		}
		view.projectName = p.Name
	}
	return view, nil
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

func (v taskView) task() component.Task {
	task := guiTask(v.detail.taskSummary, v.detail.Start.Prompt)
	task.ProjectName = v.projectName
	task.Failure = v.detail.Failure
	for _, child := range v.children {
		task.Children = append(task.Children, string(child.ID))
	}
	return task
}

// childList lists the tasks the task spawned, with each one's latest
// report.
func (v taskView) childList() html.Node {
	rows := make([]html.Node, len(v.children))
	for idx, child := range v.children {
		row := component.Child{ID: string(child.ID), Purpose: child.Purpose, Agent: child.Agent, State: string(child.State), Report: child.Report}
		if child.Branch != nil {
			pushed := transcript.BranchPushed(*child.Branch)
			row.Branch = &pushed
		}
		rows[idx] = component.ChildRow(row)
	}
	return component.Table(component.ChildColumns, "It has not spawned any tasks.", rows...)
}

// treeView shows the tree of tasks rooted at the task.
func (v taskView) treeView() html.Node {
	return component.Tree(guiTreeNode(v.tree))
}

func guiTreeNode(node taskNode) component.TreeNode {
	gui := component.TreeNode{
		ID: string(node.ID), Purpose: node.Purpose, Prompt: node.prompt, Agent: node.Agent, State: string(node.State), CostUSD: node.CostUSD,
	}
	if node.Branch != nil {
		pushed := transcript.BranchPushed(*node.Branch)
		gui.Branch = &pushed
	}
	for _, child := range node.Children {
		gui.Children = append(gui.Children, guiTreeNode(child))
	}
	return gui
}

// header is the task's header with the controls its state offers. A task
// between processes can be resumed or stopped, but has nothing running to
// pause or interrupt. A stopped or failed task can be resumed, or retried
// when it has no session to resume, until it is dismissed, which it can
// be once.
func (v taskView) header() html.Node {
	state := v.detail.State
	live := !state.Idle()
	dismissed := v.detail.DismissedAt != nil
	var dismiss html.Node
	if state.Ended() && !dismissed {
		dismiss = component.DismissForm(v.id(), "/tasks/"+v.id())
	}
	return component.TaskHeader(v.task(), html.Fragment(component.Controls(v.id(), component.ControlSet{
		Pause:     state == TaskRunning || state == TaskAwaitingPermission,
		Resume:    state == TaskPaused || state == TaskYielded || state.Ended() && v.detail.HasSession && !dismissed,
		Retry:     state.Ended() && !v.detail.HasSession && !dismissed,
		Interrupt: live,
		Stop:      !state.Ended(),
	}), dismiss))
}

// postDismissForm dismisses an ended task from the dashboard's lists and
// returns the owner to the page the form names: the task's, or the
// dashboard, listing dismissed tasks or not.
func (s *Server) postDismissForm(w http.ResponseWriter, r *http.Request) {
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
	back := r.PostForm.Get("back")
	// back is only ever one of these, so that the form cannot send the
	// browser elsewhere.
	if back != component.DashboardURL(false) && back != component.DashboardURL(true) {
		back = "/tasks/" + string(task)
	}
	_, err = s.store.dismissTask(r.Context(), task)
	switch {
	case errors.Is(err, errUnknownTask):
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	case errors.Is(err, errNotEnded):
		http.Error(w, "The task has not ended; only a stopped or failed task can be dismissed.", http.StatusConflict)
		return
	case err != nil:
		s.internalError(w, err)
		return
	}
	if !fromHTMX(r) {
		redirect(w, r, back)
		return
	}
	if back == "/tasks/"+string(task) {
		view, ok := s.taskViewFromPath(w, r)
		if !ok {
			return
		}
		s.writeHTML(w, http.StatusOK, component.OutOfBand(component.RegionTaskHeader, view.header()))
		return
	}
	lists, _, err := s.dashboardLists(r.Context(), back == component.DashboardURL(true))
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeHTML(w, http.StatusOK, component.OutOfBand(component.RegionDashboard, lists))
}

// pending returns the permission requests waiting for an answer. The
// transcript decides, not the state, which reads pausing while a request
// waits; a task with no process can no longer take an answer.
func (v taskView) pending() []transcript.PermissionRequested {
	if v.detail.State.Idle() {
		return nil
	}
	return pendingPermissions(v.entries)
}

func (v taskView) permission(problem string) html.Node {
	return component.PermissionPrompt(v.id(), v.pending(), problem)
}

// followUp is what the task's follow-up form offers: nothing while the
// task is pausing or once it is dismissed, and otherwise a prompt, which
// starts afresh a stopped or failed task that has no session to continue.
func (v taskView) followUp() component.FollowUp {
	var offer component.FollowUp
	switch {
	case v.detail.DismissedAt != nil:
		offer.Closed = "The task was dismissed; it takes no more prompts."
	case v.detail.State == TaskPausing:
		offer.Closed = "The task is pausing; prompts open again once it has paused."
	case v.detail.State.Ended() && !v.detail.HasSession:
		offer.Notes = append(offer.Notes, "The task's harness never started, so it has no session to continue. "+
			"A prompt starts it afresh, on whichever daemon fits, with its first prompt followed by yours; Retry starts it with its first prompt alone.")
	case v.detail.State == TaskRunning || v.detail.State == TaskAwaitingPermission:
		offer.Steer = true
	}
	return offer
}

// queuedList lists the task's prompts that have not reached its harness
// yet, each of which can be withdrawn. problem, when set, says why the
// last withdrawal was refused.
func (v taskView) queuedList(problem string) html.Node {
	queued := make([]component.Queued, len(v.detail.Queued))
	for idx, q := range v.detail.Queued {
		queued[idx] = component.Queued(q)
	}
	return component.QueuedPrompts(v.id(), queued, problem)
}

func (v taskView) promptForm(text, problem string) html.Node {
	return component.PromptForm(v.id(), v.followUp(), text, problem)
}

// refusal is a task form the server refused: what the owner entered and
// why it was refused, shown on the form it came from.
type refusal struct {
	kind    protocol.CommandKind
	text    string
	problem string
}

func (v taskView) page(refused refusal) html.Node {
	_, at := cursor{}.after(v.entries)
	var promptText, promptProblem, permissionProblem, withdrawProblem string
	switch refused.kind {
	case protocol.CommandPrompt:
		promptText, promptProblem = refused.text, refused.problem
	case protocol.CommandAnswerPermission:
		permissionProblem = refused.problem
	case protocol.CommandWithdraw:
		withdrawProblem = refused.problem
	}
	return component.Page("Task "+v.id(),
		component.RegionOf(component.RegionTaskHeader, v.header()),
		component.RegionOf(component.RegionPermission, v.permission(permissionProblem)),
		component.Section("Children", component.RegionOf(component.RegionChildren, v.childList())),
		component.Section("Tree", component.RegionOf(component.RegionTree, v.treeView())),
		component.Section("Transcript",
			component.RegionOf(component.RegionUnknown, v.unknownToggle()),
			component.Transcript(v.visible(v.entries)),
			component.LiveUpdates(v.id(), at.String(), v.showUnknown)),
		component.Section("Queued prompts", component.RegionOf(component.RegionQueued, v.queuedList(withdrawProblem))),
		component.Section("Follow up", component.RegionOf(component.RegionPrompt, v.promptForm(promptText, promptProblem))),
	)
}

func (s *Server) getTaskPage(w http.ResponseWriter, r *http.Request) {
	view, ok := s.taskViewFromPath(w, r)
	if !ok {
		return
	}
	view.showUnknown = showsUnknown(r)
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
	var withdrawTurn uint64
	switch refused.kind {
	case protocol.CommandPause, protocol.CommandResume, protocol.CommandInterrupt, protocol.CommandStop:
	case protocol.CommandPrompt:
		refused.text = r.PostForm.Get("text")
		if strings.TrimSpace(refused.text) == "" {
			refused.problem = "Write a prompt first."
		}
		payload = protocol.Prompt{Text: refused.text, Steer: r.PostForm.Get("steer") == component.SteerNow}
	case protocol.CommandWithdraw:
		turn, turnErr := strconv.ParseUint(r.PostForm.Get("turn"), 10, 63)
		prompt, promptErr := strconv.ParseUint(r.PostForm.Get("prompt"), 10, 63)
		switch {
		case turnErr == nil && turn > 0:
			withdrawTurn = turn
		case promptErr == nil && prompt > 0:
			payload = protocol.Withdraw{Prompt: prompt}
		default:
			http.Error(w, "withdraw needs a turn or a prompt", http.StatusBadRequest)
			return
		}
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
	if refused.problem == "" && withdrawTurn == 0 {
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
		switch {
		case refused.kind == protocol.CommandPrompt || refused.kind == protocol.CommandResume:
			_, err = s.store.queueCommand(r.Context(), task, turnKind(refused.kind), checked)
		case withdrawTurn != 0:
			err = s.store.withdrawTurn(r.Context(), task, withdrawTurn)
		case refused.kind == protocol.CommandWithdraw:
			var withdraw protocol.Withdraw
			json.Unmarshal(checked, &withdraw)
			_, err = s.store.withdrawPrompt(r.Context(), task, withdraw.Prompt)
		default:
			_, err = s.store.issueCommand(r.Context(), task, refused.kind, checked)
		}
		if errors.Is(err, errUnknownTask) {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		if errors.Is(err, errNotQueued) {
			refused.problem = "That prompt no longer waits: the harness has it, or it was withdrawn or dropped."
		} else if errors.Is(err, errDismissed) {
			refused.problem = "The task was dismissed; it takes no more commands."
		} else if errors.Is(err, errTaskEnded) {
			refused.problem = "The task has no process; send a follow-up prompt, Resume or Retry instead."
		} else if errors.Is(err, errNotStarted) {
			refused.problem = "The task has not started yet; it can only be stopped."
		} else if err != nil {
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
		switch refused.kind {
		case protocol.CommandPrompt:
			form = component.OutOfBand(component.RegionPrompt, view.promptForm(refused.text, refused.problem))
		case protocol.CommandWithdraw:
			form = component.OutOfBand(component.RegionQueued, view.queuedList(refused.problem))
		}
		s.writeHTML(w, http.StatusUnprocessableEntity, form)
		return
	}
	// The header, the permission prompt and the prompt's button are left
	// to the page's stream, or its poller, which the trigger makes poll at
	// once. Were this response to swap them too, it could arrive after a
	// newer update from the stream and leave the page showing the older
	// state.
	w.Header().Set("HX-Trigger", component.EventTaskChanged)
	var promptForm html.Node
	if refused.kind == protocol.CommandPrompt {
		promptForm = component.OutOfBand(component.RegionPrompt, view.promptForm("", ""))
	}
	s.writeHTML(w, http.StatusOK, html.Fragment(promptForm))
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

// showsUnknown reports whether the request asks for the transcript's
// unrecognised entries.
func showsUnknown(r *http.Request) bool {
	return r.URL.Query().Get(component.UnknownParam) == component.UnknownShown
}

// visible returns entries without the unrecognised ones, unless the view
// shows them.
func (v taskView) visible(entries []transcript.Entry) []transcript.Entry {
	if v.showUnknown {
		return entries
	}
	return slices.DeleteFunc(slices.Clone(entries), isUnknown)
}

func isUnknown(entry transcript.Entry) bool {
	_, unknown := entry.Body.(transcript.Unknown)
	return unknown
}

func (v taskView) unknownToggle() html.Node {
	count := 0
	for _, entry := range v.entries {
		if isUnknown(entry) {
			count++
		}
	}
	return component.UnknownToggle(v.id(), count, v.showUnknown)
}
