package component

import (
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
	ID             string
	State          string
	Daemon         string
	Model          string
	Prompt         string
	CreatedAt      time.Time
	LastActivityAt time.Time
	CostUSD        float64
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
	case "pending", "running", "pausing", "paused", "finished", "stopped", "failed":
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

// TaskColumns head a Table of TaskRows.
var TaskColumns = []string{"State", "Prompt", "Daemon", "Model", "Cost", "Last activity"}

// TaskRow is a task in the task list, linking to its page.
func TaskRow(task Task) html.Node {
	return html.El("tr", nil,
		cell(StateBadge(task.State)),
		cell(link(taskURL(task.ID), excerpt(task.Prompt))),
		cell(html.Text(task.Daemon)),
		cell(html.Text(task.Model)),
		cell(html.Text(cost(task.CostUSD))),
		cell(timestamp(task.LastActivityAt)),
	)
}

// Attention is a task waiting for the owner, and why.
type Attention struct {
	Task   Task
	Reason string
}

// AttentionList lists the tasks waiting for the owner.
func AttentionList(items []Attention) html.Node {
	if len(items) == 0 {
		return html.El("p", attrs("class", "empty"), html.Text("Nothing needs attention."))
	}
	entries := make([]html.Node, len(items))
	for idx, item := range items {
		entries[idx] = html.El("li", nil,
			StateBadge(item.Task.State), html.Text(" "),
			link(taskURL(item.Task.ID), excerpt(item.Task.Prompt)),
			html.El("span", attrs("class", "reason"), html.Text(item.Reason)))
	}
	return html.El("ul", attrs("class", "attention"), entries...)
}

// TaskHeader is what a task page says about the task, with its controls.
func TaskHeader(task Task, controls html.Node) html.Node {
	term := func(name string, value html.Node) html.Node {
		return html.Fragment(html.El("dt", nil, html.Text(name)), html.El("dd", nil, value))
	}
	return html.El("header", attrs("class", "task-header"),
		html.El("h1", nil, html.Text(excerpt(task.Prompt))),
		html.El("dl", nil,
			term("State", StateBadge(task.State)),
			term("Daemon", html.Text(task.Daemon)),
			term("Model", html.Text(task.Model)),
			term("Cost", html.Text(cost(task.CostUSD))),
			term("Created", timestamp(task.CreatedAt)),
			term("Last activity", timestamp(task.LastActivityAt)),
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

// PromptForm sends the task a follow-up prompt.
func PromptForm(taskID string, disabled bool, text, problem string) html.Node {
	return Form(commandsURL(taskID), problem,
		Field(FieldSpec{Kind: FieldHidden, Name: "kind", Value: string(protocol.CommandPrompt)}),
		Field(FieldSpec{Kind: FieldTextarea, Name: "text", Label: "Follow-up prompt", Value: text, Required: true}),
		RegionOf(RegionPromptSubmit, PromptSubmit(disabled)),
	)
}

// PromptSubmit is PromptForm's button, disabled while a pause is under
// way. It is a region of its own so that it can change without losing
// what the owner has typed.
func PromptSubmit(disabled bool) html.Node {
	if disabled {
		return html.Fragment(
			html.El("button", attrs("type", "submit", "class", string(VariantPrimary), "disabled", ""), html.Text("Send")),
			html.El("span", attrs("class", "reason"), html.Text(" The task is pausing; prompts open again once it has paused.")))
	}
	return Button("Send", VariantPrimary, "", "")
}

// NewTask is what the owner entered to start a task.
type NewTask struct {
	Prompt, Repo, Ref, Model, Daemon string
	// Acknowledge and Cleanup are the pause limits as Go durations.
	Acknowledge, Cleanup string
}

// NewTaskForm starts a task on one of daemons. created, when set, is the
// id of the task the last submission started.
func NewTaskForm(input NewTask, daemons []string, defaultModel, problem, created string) html.Node {
	var notice, noDaemons html.Node
	if created != "" {
		notice = html.El("p", attrs("class", "notice"), html.Text("Started task "), link(taskURL(created), created), html.Text("."))
	}
	if len(daemons) == 0 {
		noDaemons = html.El("p", attrs("class", "empty"), html.Text("No daemon has connected yet."))
	}
	return html.Fragment(notice, Form("/tasks", problem,
		Field(FieldSpec{Kind: FieldTextarea, Name: "prompt", Label: "Prompt", Value: input.Prompt, Required: true}),
		Field(FieldSpec{Name: "repo", Label: "Repository", Value: input.Repo, Placeholder: "none: an empty directory"}),
		Field(FieldSpec{Name: "ref", Label: "Ref", Value: input.Ref}),
		Field(FieldSpec{Name: "model", Label: "Model", Value: input.Model, Placeholder: "default: " + defaultModel}),
		Field(FieldSpec{Kind: FieldSelect, Name: "daemon", Label: "Daemon", Value: input.Daemon, Options: daemons, Required: true}),
		noDaemons,
		Details("Pause limits",
			Field(FieldSpec{Name: "acknowledge", Label: "Acknowledge within", Value: input.Acknowledge, Required: true}),
			Field(FieldSpec{Name: "cleanup", Label: "Clean up within", Value: input.Cleanup, Required: true}),
		),
		Button("Start task", VariantPrimary, "", ""),
	))
}

// Daemon is a daemon as the GUI shows it. Quota is its latest reading,
// taken at QuotaAt; nil when it has reported none.
type Daemon struct {
	ID       string
	Harness  string
	LastSeen time.Time
	Quota    *protocol.QuotaObserved
	QuotaAt  time.Time
}

// DaemonColumns head a Table of DaemonRows.
var DaemonColumns = []string{"Daemon", "Harness", "Last seen", "Quota"}

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
		cell(quota),
	)
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

// LiveUpdates keeps a task page current from cursor on, the position
// after the last entry the page shows: server-sent events append new
// entries to the transcript, and when they fail SSEFallback switches the
// page to Polling.
func LiveUpdates(taskID, cursor string) html.Node {
	return RegionOf(RegionLive,
		html.El("div", attrs("hx-ext", "sse", "sse-connect", taskURL(taskID)+"/stream?after="+url.QueryEscape(cursor),
			"sse-swap", "message", "hx-target", "#transcript", "hx-swap", "beforeend")),
		RegionOf(RegionFallback, SSEFallback(taskID, cursor)),
	)
}

func updatesURL(taskID, cursor string) string {
	return taskURL(taskID) + "/updates?after=" + url.QueryEscape(cursor)
}

// SSEFallback fetches the updates after cursor once, when server-sent
// events fail; the response replaces RegionLive with Polling.
func SSEFallback(taskID, cursor string) html.Node {
	return html.El("div", attrs("hx-get", updatesURL(taskID, cursor), "hx-trigger", "htmx:sseError from:body once",
		"hx-target", "#transcript", "hx-swap", "beforeend"))
}

// Polling fetches the updates after cursor in five seconds.
func Polling(taskID, cursor string) html.Node {
	return html.El("div", attrs("hx-get", updatesURL(taskID, cursor), "hx-trigger", "every 5s",
		"hx-target", "#transcript", "hx-swap", "beforeend"))
}
