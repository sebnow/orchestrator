package component

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// Task is a task as the GUI shows it. State is the server's name for the
// task's state.
type Task struct {
	ID string
	// Agent names the agent the task was started as; empty for none.
	Agent          string
	State          string
	Daemon         string
	Model          string
	Prompt         string
	CreatedAt      time.Time
	LastActivityAt time.Time
	CostUSD        float64
	// Parent is the id of the task that spawned this one; empty for the
	// owner's. Children are the ids of the tasks this one spawned.
	Parent   string
	Children []string
	Priority string
	Filler   bool
	// Queue is where the task's waiting turn stands; nil when none waits.
	Queue *QueuePlace
	// DismissedAt is when the owner dismissed the ended task from the
	// dashboard's lists; zero while it is not dismissed.
	DismissedAt time.Time
	// Branch is what the daemon last pushed of the task's branch; nil
	// until it pushes.
	Branch *transcript.BranchPushed
}

// QueuePlace is where a task's waiting turn stands in the scheduler's
// queue, counting from 1, and why it waits.
type QueuePlace struct {
	Position int
	Reason   string
}

func taskURL(id string) string { return "/tasks/" + url.PathEscape(id) }

func commandsURL(id string) string { return taskURL(id) + "/commands" }

func link(href, text string) html.Node {
	return html.El("a", attrs("href", href), html.Text(text))
}

func cell(children ...html.Node) html.Node { return html.El("td", nil, children...) }

// stateBadge maps a task state to its badge's class and label.
func stateBadge(state string) (class, label string) {
	switch state {
	case "queued", "pending", "running", "pausing", "paused", "yielded", "finished", "stopped", "failed":
		return "state-" + state, state
	case "awaiting_permission":
		return "state-awaiting", "awaiting permission"
	}
	return "state-unknown", state
}

// StateBadge shows a task's state.
func StateBadge(state string) html.Node {
	class, label := stateBadge(state)
	return badge(label, class)
}

// queueBadges shows the task's state and, when a turn of it waits, its
// place in the queue. A task that has not started shows only the
// latter.
func queueBadges(task Task) html.Node {
	if task.Queue == nil {
		return StateBadge(task.State)
	}
	var state html.Node
	if task.State != "queued" {
		state = html.Fragment(StateBadge(task.State), html.Text(" "))
	}
	return html.Fragment(state, badge("queued #"+strconv.Itoa(task.Queue.Position), "state-queued"))
}

// queueReason says why the task's waiting turn waits, or is empty.
func queueReason(task Task) html.Node {
	if task.Queue == nil {
		return nil
	}
	return html.El("span", attrs("class", "reason"), html.Text(" "+task.Queue.Reason))
}

// priorityLabel is the task's priority, marked when it is filler.
func priorityLabel(task Task) string {
	if task.Filler {
		return task.Priority + ", filler"
	}
	return task.Priority
}

// TaskColumns head a Table of TaskRows.
var TaskColumns = []string{"State", "Prompt", "Agent", "Priority", "Daemon", "Model", "Cost", "Last activity", "Branch"}

// TaskRow is a task in the task list, linking to its page.
func TaskRow(task Task) html.Node {
	return html.El("tr", nil,
		cell(queueBadges(task), queueReason(task), dismissedMark(task)),
		cell(link(taskURL(task.ID), excerpt(task.Prompt)), lineage(task.Parent)),
		cell(agentLink(task.Agent)),
		cell(html.Text(priorityLabel(task))),
		cell(html.Text(task.Daemon)),
		cell(html.Text(task.Model)),
		cell(html.Text(cost(task.CostUSD))),
		cell(timestamp(task.LastActivityAt)),
		cell(branchCell(task.Branch)),
	)
}

// agentLink links the agent name, or is empty for none.
func agentLink(name string) html.Node {
	if name == "" {
		return nil
	}
	return link(agentURL(name), name)
}

// dismissedMark marks a dismissed task, or is empty.
func dismissedMark(task Task) html.Node {
	if task.DismissedAt.IsZero() {
		return nil
	}
	return html.El("span", attrs("class", "reason"), html.Text(" dismissed"))
}

// lineage marks a task spawned by parent, or is empty for the owner's.
func lineage(parent string) html.Node {
	if parent == "" {
		return nil
	}
	return html.El("span", attrs("class", "reason"), html.Text(" ↳ child of "), link(taskURL(parent), parent))
}

// Attention is a task waiting for the owner, and why. Dismissable says
// the owner may dismiss it from the list.
type Attention struct {
	Task        Task
	Reason      string
	Dismissable bool
}

// AttentionList lists the tasks waiting for the owner. Dismissing one
// returns the owner to back.
func AttentionList(items []Attention, back string) html.Node {
	if len(items) == 0 {
		return html.El("p", attrs("class", "empty"), html.Text("Nothing needs attention."))
	}
	entries := make([]html.Node, len(items))
	for idx, item := range items {
		entries[idx] = html.El("li", nil,
			StateBadge(item.Task.State), html.Text(" "),
			link(taskURL(item.Task.ID), excerpt(item.Task.Prompt)),
			html.El("span", attrs("class", "reason"), html.Text(item.Reason)),
			dismissIf(item.Dismissable, item.Task.ID, back))
	}
	return html.El("ul", attrs("class", "attention"), entries...)
}

// TaskHeader is what a task page says about the task, with its controls.
func TaskHeader(task Task, controls html.Node) html.Node {
	term := func(name string, value html.Node) html.Node {
		return html.Fragment(html.El("dt", nil, html.Text(name)), html.El("dd", nil, value))
	}
	var parent, children, queue, dismissed, agent html.Node
	if task.Agent != "" {
		agent = term("Agent", agentLink(task.Agent))
	}
	var branch html.Node
	if task.Branch != nil {
		branch = term("Branch", branchDetail(*task.Branch))
	}
	if !task.DismissedAt.IsZero() {
		dismissed = term("Dismissed", timestamp(task.DismissedAt))
	}
	if task.Queue != nil {
		queue = term("Waits", html.Text(task.Queue.Reason))
	}
	if task.Parent != "" {
		parent = term("Parent", link(taskURL(task.Parent), task.Parent))
	}
	if len(task.Children) > 0 {
		links := make([]html.Node, 0, 2*len(task.Children))
		for idx, child := range task.Children {
			if idx > 0 {
				links = append(links, html.Text(", "))
			}
			links = append(links, link(taskURL(child), child))
		}
		children = term("Children", html.Fragment(links...))
	}
	return html.El("header", attrs("class", "task-header"),
		html.El("h1", nil, link(taskURL(task.ID), excerpt(task.Prompt))),
		html.El("dl", nil,
			term("State", queueBadges(task)),
			queue,
			agent,
			term("Priority", html.Text(task.Priority)),
			term("Filler", html.Text(yesNo(task.Filler))),
			term("Daemon", html.Text(task.Daemon)),
			term("Model", html.Text(task.Model)),
			term("Cost", html.Text(cost(task.CostUSD))),
			branch,
			term("Created", timestamp(task.CreatedAt)),
			term("Last activity", timestamp(task.LastActivityAt)),
			parent,
			children,
			dismissed,
		),
		controls,
		html.El("p", nil, link(taskURL(task.ID)+"/raw", "Stored events")),
	)
}

// ControlSet says which controls a task offers.
type ControlSet struct {
	Pause, Resume, Interrupt, Stop bool
}

// Controls are the buttons that pause, resume, interrupt or stop a task,
// those of offered only.
func Controls(taskID string, offered ControlSet) html.Node {
	var buttons []html.Node
	add := func(on bool, label string, variant Variant, kind protocol.CommandKind) {
		if on {
			buttons = append(buttons, Button(label, variant, "kind", string(kind)))
		}
	}
	add(offered.Pause, "Pause", VariantPlain, protocol.CommandPause)
	add(offered.Resume, "Resume", VariantPrimary, protocol.CommandResume)
	add(offered.Interrupt, "Interrupt", VariantDanger, protocol.CommandInterrupt)
	add(offered.Stop, "Stop", VariantDanger, protocol.CommandStop)
	if len(buttons) == 0 {
		return nil
	}
	return Form(commandsURL(taskID), "", html.El("div", attrs("class", "controls"), buttons...))
}

// DismissForm dismisses the ended task taskID from the dashboard's
// lists, and returns the owner to back.
func DismissForm(taskID, back string) html.Node {
	return html.El("div", attrs("class", "dismiss"), Form(taskURL(taskID)+"/dismiss", "",
		Field(FieldSpec{Kind: FieldHidden, Name: "back", Value: back}),
		Button("Dismiss", VariantPlain, "", "")))
}

func dismissIf(on bool, taskID, back string) html.Node {
	if !on {
		return nil
	}
	return DismissForm(taskID, back)
}

// DismissedParam set to DismissedShown makes the dashboard list
// dismissed tasks.
const (
	DismissedParam = "dismissed"
	DismissedShown = "show"
)

// DashboardURL is the dashboard, listing dismissed tasks when shown is
// set.
func DashboardURL(shown bool) string {
	if shown {
		return "/?" + DismissedParam + "=" + DismissedShown
	}
	return "/"
}

// DismissedToggle says how many dismissed tasks the task list hides, or
// shows when shown is set, with a link that flips it. It is empty when
// there are none.
func DismissedToggle(count int, shown bool) html.Node {
	if count == 0 {
		return nil
	}
	noun := "tasks"
	if count == 1 {
		noun = "task"
	}
	if shown {
		return html.El("p", attrs("class", "notice"),
			html.Text(fmt.Sprintf("Showing %d dismissed %s. ", count, noun)),
			link(DashboardURL(false), "Hide them"))
	}
	return html.El("p", attrs("class", "notice"),
		html.Text(fmt.Sprintf("%d dismissed %s hidden. ", count, noun)),
		link(DashboardURL(true), "Show them"))
}

// decisionVariant maps an answer to a permission request to its button.
func decisionVariant(allow bool) Variant {
	if allow {
		return VariantPrimary
	}
	return VariantDanger
}

// PermissionPrompt asks the owner to allow or deny each pending request.
// problem, when set, says why the last answer was refused.
func PermissionPrompt(taskID string, pending []transcript.PermissionRequested, problem string) html.Node {
	cards := make([]html.Node, len(pending))
	for idx, request := range pending {
		cards[idx] = Card(
			html.El("h2", nil, html.Text("Permission requested")),
			html.El("p", nil, html.Text("The agent asks to run "), html.El("code", nil, html.Text(request.Tool)), html.Text(":")),
			preformatted(prettyJSON(request.Input)),
			Form(commandsURL(taskID), problem,
				Field(FieldSpec{Kind: FieldHidden, Name: "kind", Value: string(protocol.CommandAnswerPermission)}),
				Field(FieldSpec{Kind: FieldHidden, Name: "request_id", Value: request.RequestID}),
				Field(FieldSpec{Name: "message", Label: "Reason, if denying"}),
				html.El("div", attrs("class", "controls"),
					Button("Allow", decisionVariant(true), "decision", "allow"),
					Button("Deny", decisionVariant(false), "decision", "deny")),
			),
		)
	}
	return html.Fragment(cards...)
}

// PromptForm sends the task a follow-up prompt. closed, when set, says why
// the task takes no prompt now, and disables the form's button.
func PromptForm(taskID, closed, text, problem string) html.Node {
	return Form(commandsURL(taskID), problem,
		Field(FieldSpec{Kind: FieldHidden, Name: "kind", Value: string(protocol.CommandPrompt)}),
		Field(FieldSpec{Kind: FieldTextarea, Name: "text", Label: "Follow-up prompt", Value: text, Required: true}),
		RegionOf(RegionPromptSubmit, PromptSubmit(closed)),
	)
}

// PromptSubmit is PromptForm's button, disabled with the reason closed
// when that is set. It is a region of its own so that it can change
// without losing what the owner has typed.
func PromptSubmit(closed string) html.Node {
	if closed != "" {
		return html.Fragment(
			html.El("button", attrs("type", "submit", "class", string(VariantPrimary), "disabled", ""), html.Text("Send")),
			html.El("span", attrs("class", "reason"), html.Text(" "+closed)))
	}
	return Button("Send", VariantPrimary, "", "")
}

// NewTask is what the owner entered to start a task. An empty Agent is
// none, and an empty Daemon any connected daemon. What is left empty,
// or Filler unticked, takes the agent's value, or the default.
type NewTask struct {
	Agent                            string
	Prompt, Repo, Ref, Model, Daemon string
	// Acknowledge and Cleanup are the pause limits as Go durations.
	Acknowledge, Cleanup string
	Priority             string
	// Filler is "on" when the task is filler.
	Filler string
}

// Priorities are the priorities a task can have, lowest first.
var Priorities = []string{"low", "normal", "high"}

// NewTaskForm starts a task, as one of agents or none, on one of
// daemons, or on any. The defaults are what a field left empty without
// an agent gives. created, when set, is the id of the task the last
// submission started.
func NewTaskForm(input NewTask, daemons, agents []string, defaultModel, defaultAcknowledge, defaultCleanup, problem, created string) html.Node {
	var notice, noDaemons html.Node
	if created != "" {
		notice = html.El("p", attrs("class", "notice"), html.Text("Started task "), link(taskURL(created), created), html.Text("."))
	}
	if len(daemons) == 0 {
		noDaemons = html.El("p", attrs("class", "empty"), html.Text("No daemon has connected yet."))
	}
	daemonOptions := []Option{{Value: "", Label: "Any connected daemon"}}
	for _, daemon := range daemons {
		daemonOptions = append(daemonOptions, Option{Value: daemon, Label: daemon})
	}
	agentOptions := []Option{{Value: "", Label: "None"}}
	for _, agent := range agents {
		agentOptions = append(agentOptions, Option{Value: agent, Label: agent})
	}
	priorityOptions := []Option{{Value: "", Label: "The agent's, or normal"}}
	for _, priority := range Priorities {
		priorityOptions = append(priorityOptions, Option{Value: priority, Label: priority})
	}
	return html.Fragment(notice, Form("/tasks", problem,
		Field(FieldSpec{Kind: FieldSelect, Name: "agent", Label: "Agent", Value: input.Agent, Options: agentOptions}),
		Field(FieldSpec{Kind: FieldTextarea, Name: "prompt", Label: "Prompt", Value: input.Prompt, Required: true}),
		Field(FieldSpec{Name: "repo", Label: "Repository (https:// only)", Value: input.Repo, Placeholder: "none: an empty directory"}),
		Field(FieldSpec{Name: "ref", Label: "Ref", Value: input.Ref}),
		Field(FieldSpec{Name: "model", Label: "Model", Value: input.Model, Placeholder: "the agent's, or " + defaultModel}),
		Field(FieldSpec{Kind: FieldSelect, Name: "daemon", Label: "Daemon", Value: input.Daemon, Options: daemonOptions}),
		noDaemons,
		Field(FieldSpec{Kind: FieldSelect, Name: "priority", Label: "Priority", Value: input.Priority, Options: priorityOptions}),
		Field(FieldSpec{Kind: FieldCheckbox, Name: "filler", Label: "Filler: runs only on spare budget, and yields to other work; unticked leaves the agent's choice", Value: input.Filler}),
		Details("Pause limits",
			Field(FieldSpec{Name: "acknowledge", Label: "Acknowledge within", Value: input.Acknowledge, Placeholder: "the agent's, or " + defaultAcknowledge}),
			Field(FieldSpec{Name: "cleanup", Label: "Clean up within", Value: input.Cleanup, Placeholder: "the agent's, or " + defaultCleanup}),
		),
		Button("Start task", VariantPrimary, "", ""),
	))
}

// Daemon is a daemon as the GUI shows it. Quota is its latest reading,
// taken at QuotaAt; nil when it has reported none. InUse of its Slots
// are held by tasks. LostSince is when the server declared it lost; zero
// while it is not.
type Daemon struct {
	ID        string
	Harness   string
	LastSeen  time.Time
	Connected bool
	LostSince time.Time
	Slots     int
	InUse     int
	Quota     *protocol.QuotaObserved
	QuotaAt   time.Time
}

// DaemonColumns head a Table of DaemonRows.
var DaemonColumns = []string{"Daemon", "Harness", "Last seen", "Connected", "Slots", "Quota"}

// DaemonRow is a daemon in the daemon list.
func DaemonRow(daemon Daemon) html.Node {
	quota := html.Text("no reading yet")
	if daemon.Quota != nil {
		quota = QuotaReadout(*daemon.Quota, daemon.QuotaAt)
	}
	return html.El("tr", nil,
		cell(html.Text(daemon.ID)),
		cell(html.Text(daemon.Harness)),
		cell(timestamp(daemon.LastSeen)),
		cell(connection(daemon)),
		cell(html.Text(fmt.Sprintf("%d of %d in use", daemon.InUse, daemon.Slots))),
		cell(quota),
	)
}

// connection says whether daemon is connected, and since when it is lost
// if it is.
func connection(daemon Daemon) html.Node {
	if !daemon.Connected && !daemon.LostSince.IsZero() {
		return html.Fragment(html.Text("no, lost since "), timestamp(daemon.LostSince))
	}
	return html.Text(yesNo(daemon.Connected))
}

// Budget is the account's quota reading as the scheduler uses it: quota,
// taken at at and age old, or nil when there is none; and the five-hour
// utilization that filler and low-priority turns must stay below.
func Budget(quota *protocol.QuotaObserved, at time.Time, age time.Duration, fillerBelow, lowBelow float64) html.Node {
	thresholds := html.El("p", attrs("class", "notice"), html.Text(fmt.Sprintf(
		"Filler runs while the five-hour window is below %s used, low priority below %s; "+
			"a rejected reading holds every turn until its window resets.", percent(fillerBelow), percent(lowBelow))))
	if quota == nil {
		return html.Fragment(html.El("p", attrs("class", "empty"), html.Text("No reading yet, so filler waits for one.")), thresholds)
	}
	return html.Fragment(QuotaReadout(*quota, at),
		html.El("p", attrs("class", "notice"), html.Text("Taken "+ago(age)+" ago, by the newest turn on any daemon.")),
		thresholds)
}

func percent(fraction float64) string {
	return strconv.FormatFloat(fraction*100, 'f', 0, 64) + "%"
}

// ago words an age in minutes, hours past two hours, or days past two
// days.
func ago(age time.Duration) string {
	switch {
	case age < time.Minute:
		return "less than a minute"
	case age < 2*time.Hour:
		return plural(int(age/time.Minute), "minute")
	case age < 48*time.Hour:
		return plural(int(age/time.Hour), "hour")
	}
	return plural(int(age/(24*time.Hour)), "day")
}

func plural(count int, unit string) string {
	if count == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(count) + " " + unit + "s"
}

func yesNo(on bool) string {
	if on {
		return "yes"
	}
	return "no"
}

// QuotaReadout shows a usage-limit reading: its status and how much of
// each window is used. at, when set, is when it was taken.
func QuotaReadout(quota protocol.QuotaObserved, at time.Time) html.Node {
	windows := make([]html.Node, len(quota.Windows))
	for idx, window := range quota.Windows {
		windows[idx] = html.El("li", nil,
			html.Text(strings.ReplaceAll(window.Name, "_", " ")+": "),
			html.El("meter", attrs("min", "0", "max", "1", "value", strconv.FormatFloat(window.Utilization, 'f', -1, 64))),
			html.Text(" "+strconv.FormatFloat(window.Utilization*100, 'f', 0, 64)+"% used, resets "), timestamp(window.ResetsAt))
	}
	var taken html.Node
	if !at.IsZero() {
		taken = html.Fragment(html.Text(" as of "), timestamp(at))
	}
	return html.El("div", attrs("class", "quota"),
		badge(string(quota.Status), "quota-"+string(quota.Status)), taken,
		html.El("ul", nil, windows...))
}

// EventColumns head a Table of EventRows.
var EventColumns = []string{"Seq", "Kind", "Time", "Payload"}

// EventRow is one stored event, its payload as stored.
func EventRow(event protocol.Event) html.Node {
	return html.El("tr", nil,
		cell(html.Text(strconv.FormatUint(event.Seq, 10))),
		cell(html.Text(string(event.Kind))),
		cell(timestamp(event.Time)),
		cell(html.El("pre", attrs("class", "payload"), html.Text(string(event.Payload)))),
	)
}

// UnknownParam set to UnknownShown makes a task page, and the updates
// that keep it live, show the transcript's unrecognised entries.
const (
	UnknownParam = "unknown"
	UnknownShown = "show"
	unknownQuery = UnknownParam + "=" + UnknownShown
)

// UnknownToggle says how many unrecognised entries the transcript hides,
// or shows when shown is set, with a link that flips it. It is empty when
// there are none.
func UnknownToggle(taskID string, count int, shown bool) html.Node {
	if count == 0 {
		return nil
	}
	noun := "entries"
	if count == 1 {
		noun = "entry"
	}
	if shown {
		return html.El("p", attrs("class", "notice"),
			html.Text(fmt.Sprintf("Showing %d unrecognised %s. ", count, noun)),
			link(taskURL(taskID), "Hide them"))
	}
	return html.El("p", attrs("class", "notice"),
		html.Text(fmt.Sprintf("%d unrecognised %s hidden. ", count, noun)),
		link(taskURL(taskID)+"?"+unknownQuery, "Show them"))
}

// liveQuery is the query of a request for the updates after cursor.
func liveQuery(cursor string, showUnknown bool) string {
	query := "after=" + url.QueryEscape(cursor)
	if showUnknown {
		query += "&" + unknownQuery
	}
	return query
}

// LiveUpdates keeps a task page current from cursor on, the position
// after the last entry the page shows: server-sent events append new
// entries to the transcript, and when they fail SSEFallback switches the
// page to Polling. showUnknown keeps unrecognised entries in the updates.
func LiveUpdates(taskID, cursor string, showUnknown bool) html.Node {
	return RegionOf(RegionLive,
		html.El("div", attrs("hx-ext", "sse", "sse-connect", taskURL(taskID)+"/stream?"+liveQuery(cursor, showUnknown),
			"sse-swap", "message", "hx-target", "#transcript", "hx-swap", "beforeend")),
		RegionOf(RegionFallback, SSEFallback(taskID, cursor, showUnknown)),
	)
}

func updatesURL(taskID, cursor string, showUnknown bool) string {
	return taskURL(taskID) + "/updates?" + liveQuery(cursor, showUnknown)
}

// SSEFallback fetches the updates after cursor once, when server-sent
// events fail; the response replaces RegionLive with Polling.
func SSEFallback(taskID, cursor string, showUnknown bool) html.Node {
	return html.El("div", attrs("hx-get", updatesURL(taskID, cursor, showUnknown), "hx-trigger", "htmx:sseError from:body once",
		"hx-target", "#transcript", "hx-swap", "beforeend"))
}

// EventTaskChanged is the event a task page's form response triggers,
// through the HX-Trigger header, once it has changed the task.
const EventTaskChanged = "task-changed"

// Polling fetches the updates after cursor in five seconds, or at once
// when a form has changed the task.
func Polling(taskID, cursor string, showUnknown bool) html.Node {
	return html.El("div", attrs("hx-get", updatesURL(taskID, cursor, showUnknown), "hx-trigger", "every 5s, "+EventTaskChanged+" from:body",
		"hx-target", "#transcript", "hx-swap", "beforeend"))
}

// branchCell is a task's branch in the task list, marked when its last
// push failed.
func branchCell(pushed *transcript.BranchPushed) html.Node {
	if pushed == nil {
		return nil
	}
	var failed html.Node
	if pushed.Error != "" {
		failed = html.Fragment(html.Text(" "), badge("push failed", "state-failed"))
	}
	return html.Fragment(html.El("code", nil, html.Text(pushed.Branch)), failed)
}

// branchDetail is what a task page says of the task's branch: its name,
// the commit last pushed or tried, and why the push failed if it did.
func branchDetail(pushed transcript.BranchPushed) html.Node {
	verb := " at "
	if pushed.Error != "" {
		verb = ", push failed at "
	}
	return html.Fragment(
		html.El("code", nil, html.Text(pushed.Branch)), html.Text(verb), html.El("code", nil, html.Text(shortCommit(pushed.Commit))),
		html.Text(", "+branchCounts(pushed.Ahead, pushed.Uncommitted)),
		errorText(pushed.Error))
}
