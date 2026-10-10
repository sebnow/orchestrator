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
	Agent string
	// Project is the id of the project the task belongs to, and
	// ProjectName its name; both empty for none. An empty ProjectName
	// shows the id.
	Project, ProjectName string
	// Purpose says why the task exists; empty for none.
	Purpose string
	// Requires are the labels the task's daemon must have.
	Requires       map[string]string
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
	// Failure says why a failed task failed; empty otherwise.
	Failure string
	// ContextTokens is the size of the task's session as of its latest
	// turn's end, and ContextWindow its model's context window, both in
	// tokens; zero when unknown.
	ContextTokens, ContextWindow int64
	// Continues is the id of the task this one continues, its
	// predecessor; empty for none. ContinuedBy are the ids of the tasks
	// that continue this one.
	Continues   string
	ContinuedBy []string
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
		cell(link(taskURL(task.ID), title(task.Purpose, task.Prompt)), lineage(task.Parent)),
		cell(agentLink(task.Agent)),
		cell(html.Text(priorityLabel(task))),
		cell(html.Text(task.Daemon)),
		cell(html.Text(task.Model)),
		cell(html.Text(cost(task.CostUSD))),
		cell(timestamp(task.LastActivityAt)),
		cell(branchCell(task.Branch)),
	)
}

// title names a task in a list: its purpose when it has one, which says
// why it exists, or else the start of its prompt.
func title(purpose, prompt string) string {
	if purpose != "" {
		return excerpt(purpose)
	}
	return excerpt(prompt)
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

// Child is a task another spawned, as its parent's page lists it.
// Purpose is why its parent spawned it. Report is the latest message it
// sent its parent; empty for none. Branch is what the daemon last pushed
// of its branch; nil for none.
type Child struct {
	ID, Purpose, Agent, State, Report string
	Branch                            *transcript.BranchPushed
}

// ChildColumns head a Table of ChildRows.
var ChildColumns = []string{"Task", "Purpose", "Agent", "State", "Branch", "Latest report"}

// ChildRow is a child in its parent's list, linking to its page.
func ChildRow(child Child) html.Node {
	report := html.Node(html.El("span", attrs("class", "empty"), html.Text("none yet")))
	if child.Report != "" {
		report = html.Text(excerpt(child.Report))
	}
	purpose := html.Node(html.El("span", attrs("class", "empty"), html.Text("none given")))
	if child.Purpose != "" {
		purpose = html.Text(child.Purpose)
	}
	return html.El("tr", nil,
		cell(link(taskURL(child.ID), child.ID)),
		cell(purpose),
		cell(agentLink(child.Agent)),
		cell(StateBadge(child.State)),
		cell(childBranch(child.Branch)),
		cell(report),
	)
}

// childBranch is a child's branch and the commit last pushed or tried,
// marked when the push failed.
func childBranch(pushed *transcript.BranchPushed) html.Node {
	if pushed == nil {
		return html.El("span", attrs("class", "empty"), html.Text("none"))
	}
	var failed html.Node
	if pushed.Error != "" {
		failed = html.Fragment(html.Text(" "), badge("push failed", "state-failed"))
	}
	return html.Fragment(html.El("code", nil, html.Text(pushed.Branch)),
		html.Text(" at "), html.El("code", nil, html.Text(shortCommit(pushed.Commit))), failed)
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
	var parent, children, queue, dismissed, agent, requires, project, purpose html.Node
	if task.Purpose != "" {
		purpose = term("Purpose", html.Text(task.Purpose))
	}
	if task.Project != "" {
		project = term("Project", projectLink(task.Project, task.ProjectName))
	}
	if len(task.Requires) > 0 {
		requires = term("Requires", LabelList(task.Requires))
	}
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
	var failure, context, continues, continuedBy html.Node
	if task.Failure != "" {
		failure = term("Failure", html.Text(task.Failure))
	}
	if task.ContextTokens > 0 {
		context = term("Context", html.Text(ContextSize(task.ContextTokens, task.ContextWindow)))
	}
	if task.Continues != "" {
		continues = term("Continues", link(taskURL(task.Continues), task.Continues))
	}
	if len(task.ContinuedBy) > 0 {
		continuedBy = term("Continued by", taskLinks(task.ContinuedBy))
	}
	if task.Parent != "" {
		parent = term("Parent", link(taskURL(task.Parent), task.Parent))
	}
	if len(task.Children) > 0 {
		children = term("Children", taskLinks(task.Children))
	}
	return html.El("header", attrs("class", "task-header"),
		html.El("h1", nil, link(taskURL(task.ID), excerpt(task.Prompt))),
		html.El("dl", nil,
			purpose,
			term("State", queueBadges(task)),
			failure,
			queue,
			project,
			agent,
			requires,
			term("Priority", html.Text(task.Priority)),
			term("Filler", html.Text(yesNo(task.Filler))),
			term("Daemon", html.Text(task.Daemon)),
			term("Model", html.Text(task.Model)),
			term("Cost", html.Text(cost(task.CostUSD))),
			context,
			branch,
			term("Created", timestamp(task.CreatedAt)),
			term("Last activity", timestamp(task.LastActivityAt)),
			parent,
			children,
			continues,
			continuedBy,
			dismissed,
		),
		controls,
		html.El("p", nil, link(taskURL(task.ID)+"/raw", "Stored events")),
	)
}

// ControlSet says which controls a task offers. Retry resumes a task that
// has no session to continue, which starts it afresh.
type ControlSet struct {
	Pause, Resume, Retry, Interrupt, Stop bool
}

// Controls are the buttons that pause, resume, retry, interrupt or stop a
// task, those of offered only.
func Controls(taskID string, offered ControlSet) html.Node {
	var buttons []html.Node
	add := func(on bool, label string, variant Variant, kind protocol.CommandKind) {
		if on {
			buttons = append(buttons, Button(label, variant, "kind", string(kind)))
		}
	}
	add(offered.Pause, "Pause", VariantPlain, protocol.CommandPause)
	add(offered.Resume, "Resume", VariantPrimary, protocol.CommandResume)
	add(offered.Retry, "Retry", VariantPrimary, protocol.CommandResume)
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

// FollowUp is what a task's follow-up form offers as the task stands.
// Closed, when set, says why the task takes no prompt now. Notes say what
// sending one will do. Steer offers, besides sending the prompt after the
// running turn, sending it now, which interrupts the turn.
type FollowUp struct {
	Closed string
	Notes  []string
	Steer  bool
}

// PromptForm sends the task a follow-up prompt, as offer allows.
func PromptForm(taskID string, offer FollowUp, text, problem string) html.Node {
	return Form(commandsURL(taskID), problem,
		Field(FieldSpec{Kind: FieldHidden, Name: "kind", Value: string(protocol.CommandPrompt)}),
		Field(FieldSpec{Kind: FieldTextarea, Name: "text", Label: "Follow-up prompt", Value: text, Required: true}),
		RegionOf(RegionPromptSubmit, PromptSubmit(offer)),
	)
}

// PromptSubmit is PromptForm's notes and button, disabled with the reason
// offer.Closed when that is set. It is a region of its own so that it can
// change without losing what the owner has typed.
func PromptSubmit(offer FollowUp) html.Node {
	notes := make([]html.Node, len(offer.Notes))
	for idx, note := range offer.Notes {
		notes[idx] = html.El("p", attrs("class", "notice"), html.Text(note))
	}
	if offer.Closed != "" {
		return html.Fragment(html.Fragment(notes...),
			html.El("button", attrs("type", "submit", "class", string(VariantPrimary), "disabled", ""), html.Text("Send")),
			html.El("span", attrs("class", "reason"), html.Text(" "+offer.Closed)))
	}
	if offer.Steer {
		return html.Fragment(html.Fragment(notes...),
			html.El("p", attrs("class", "notice"), html.Text("A turn is running. After this turn, the daemon holds the prompt, "+
				"which can be withdrawn until the turn ends, and then sends it; now, it interrupts the turn and sends the prompt "+
				"as the next one in the same session.")),
			html.El("div", attrs("class", "controls"),
				Button("Send after this turn", VariantPrimary, "steer", ""),
				Button("Send now", VariantDanger, "steer", SteerNow)))
	}
	return html.Fragment(html.Fragment(notes...), Button("Send", VariantPrimary, "", ""))
}

// SteerNow is the value of the follow-up form's steer field that sends
// the prompt now, interrupting the running turn.
const SteerNow = "now"

// Queued is a task's prompt that has not reached its harness yet: a
// turn waiting for the scheduler, Turn, or a prompt the daemon holds
// until the running turn ends, Prompt.
type Queued struct {
	Turn, Prompt uint64
	Text         string
	Since        time.Time
}

// QueuedPrompts lists queued, each with a button that withdraws it.
// problem, when set, says why the last withdrawal was refused.
func QueuedPrompts(taskID string, queued []Queued, problem string) html.Node {
	refused := problemNote(problem)
	if len(queued) == 0 {

		return html.Fragment(refused, html.El("p", attrs("class", "empty"), html.Text("No prompt waits.")))
	}
	items := make([]html.Node, len(queued))
	for idx, q := range queued {
		where, field, id := "waits for the scheduler", "turn", q.Turn
		if q.Prompt != 0 {
			where, field, id = "held by the daemon until the turn ends", "prompt", q.Prompt
		}
		items[idx] = html.El("li", nil,
			timestamp(q.Since), html.Text(" "), html.El("span", attrs("class", "reason"), html.Text(where)),
			preformatted(q.Text),
			Form(commandsURL(taskID), "",
				Field(FieldSpec{Kind: FieldHidden, Name: "kind", Value: string(protocol.CommandWithdraw)}),
				Field(FieldSpec{Kind: FieldHidden, Name: field, Value: strconv.FormatUint(id, 10)}),
				Button("Withdraw", VariantPlain, "", "")))
	}
	return html.Fragment(refused, html.El("ul", attrs("class", "queued"), items...))
}

// NewTask is what the owner entered to start a task. An empty Agent is
// none, or the project's default agent, an empty Project none, and an
// empty Daemon any connected daemon. What is left empty, or Filler
// unticked, takes the agent's value, or the default.
type NewTask struct {
	Agent, Project string
	// Purpose says why the task exists; empty for none.
	Purpose string
	// Requires are key=value labels the task's daemon must have.
	Requires                         string
	Prompt, Repo, Ref, Model, Daemon string
	// Acknowledge and Cleanup are the pause limits as Go durations.
	Acknowledge, Cleanup string
	Priority             string
	// Filler is "on" when the task is filler.
	Filler string
}

// Priorities are the priorities a task can have, lowest first.
var Priorities = []string{"low", "normal", "high"}

// TaskChoices are what the new-task forms offer: the daemons, the agents
// and the projects, by id and name, and what a field left empty without
// an agent gives.
type TaskChoices struct {
	Daemons, Agents                                  []string
	Projects                                         []Option
	DefaultModel, DefaultAcknowledge, DefaultCleanup string
}

// NewTaskForm starts a task, in one of the projects or none, as one of
// the agents or none, on one of the daemons, or on any. created, when
// set, is the id of the task the last submission started.
func NewTaskForm(input NewTask, choices TaskChoices, problem, created string) html.Node {
	var notice html.Node
	if created != "" {
		notice = html.El("p", attrs("class", "notice"), html.Text("Started task "), link(taskURL(created), created), html.Text("."))
	}
	// Without projects the form offers none, and says nothing of them.
	var project html.Node
	noAgent, noRepo := "None", "none: an empty directory"
	if len(choices.Projects) > 0 {
		projects := append([]Option{{Value: "", Label: "None"}}, choices.Projects...)
		project = Field(FieldSpec{Kind: FieldSelect, Name: "project", Label: "Project", Value: input.Project, Options: projects})
		noAgent, noRepo = "None, or the project's default", "the project's, or none: an empty directory"
	}
	workspace := html.Fragment(
		Field(FieldSpec{Name: "repo", Label: "Repository (https:// or ssh:// URL, or ssh address such as git@host:path)", Value: input.Repo, Placeholder: noRepo}),
		Field(FieldSpec{Name: "ref", Label: "Ref", Value: input.Ref}),
	)
	return html.Fragment(notice, Form("/tasks", problem,
		append([]html.Node{project}, taskFields(input, choices, noAgent, workspace)...)...))
}

// ProjectTaskForm starts a task in project, as one of the agents or the
// project's default, in the project's repository, or when it has none,
// in the one given. Unlike NewTaskForm it leaves the browser to follow
// the response, the new task's page.
func ProjectTaskForm(input NewTask, choices TaskChoices, project ProjectInput) html.Node {
	noAgent := "None"
	if project.DefaultAgent != "" {
		noAgent = "The project's default, " + project.DefaultAgent
	}
	workspace := html.El("p", nil, html.Text("Repository: "), repository(project.Repo, project.Ref), html.Text(", the project's."))
	if project.Repo == "" {
		workspace = html.Fragment(
			Field(FieldSpec{Name: "repo", Label: "Repository (https:// or ssh:// URL, or ssh address such as git@host:path)", Value: input.Repo, Placeholder: "none: an empty directory"}),
			Field(FieldSpec{Name: "ref", Label: "Ref", Value: input.Ref}),
		)
	}
	return PlainForm("/tasks", "",
		append([]html.Node{Field(FieldSpec{Kind: FieldHidden, Name: "project", Value: project.ID})},
			taskFields(input, choices, noAgent, workspace)...)...)
}

// taskFields are the new-task forms' fields after the project: the
// agent, whose empty choice is labelled noAgent, the prompt, workspace,
// and the rest.
func taskFields(input NewTask, choices TaskChoices, noAgent string, workspace html.Node) []html.Node {
	var noDaemons html.Node
	if len(choices.Daemons) == 0 {
		noDaemons = html.El("p", attrs("class", "empty"), html.Text("No daemon has connected yet."))
	}
	daemonOptions := []Option{{Value: "", Label: "Any connected daemon"}}
	for _, daemon := range choices.Daemons {
		daemonOptions = append(daemonOptions, Option{Value: daemon, Label: daemon})
	}
	agentOptions := []Option{{Value: "", Label: noAgent}}
	for _, agent := range choices.Agents {
		agentOptions = append(agentOptions, Option{Value: agent, Label: agent})
	}
	priorityOptions := []Option{{Value: "", Label: "The agent's, or normal"}}
	for _, priority := range Priorities {
		priorityOptions = append(priorityOptions, Option{Value: priority, Label: priority})
	}
	return []html.Node{
		Field(FieldSpec{Kind: FieldSelect, Name: "agent", Label: "Agent", Value: input.Agent, Options: agentOptions}),
		Field(FieldSpec{Name: "purpose", Label: "Purpose, one line on why the task exists; put at the top of its prompt", Value: input.Purpose, Placeholder: "none"}),
		Field(FieldSpec{Kind: FieldTextarea, Name: "prompt", Label: "Prompt", Value: input.Prompt, Required: true}),
		workspace,
		Field(FieldSpec{Name: "model", Label: "Model", Value: input.Model, Placeholder: "the agent's, or " + choices.DefaultModel}),
		Field(FieldSpec{Kind: FieldSelect, Name: "daemon", Label: "Daemon", Value: input.Daemon, Options: daemonOptions}),
		Field(FieldSpec{Name: "requires", Label: "Requires labels (key=value, separated by commas)", Value: input.Requires, Placeholder: "the agent's, or none"}),
		noDaemons,
		Field(FieldSpec{Kind: FieldSelect, Name: "priority", Label: "Priority", Value: input.Priority, Options: priorityOptions}),
		Field(FieldSpec{Kind: FieldCheckbox, Name: "filler", Label: "Filler: runs only on spare budget, and yields to other work; unticked leaves the agent's choice", Value: input.Filler}),
		Details("Pause limits",
			Field(FieldSpec{Name: "acknowledge", Label: "Acknowledge within", Value: input.Acknowledge, Placeholder: "the agent's, or " + choices.DefaultAcknowledge}),
			Field(FieldSpec{Name: "cleanup", Label: "Clean up within", Value: input.Cleanup, Placeholder: "the agent's, or " + choices.DefaultCleanup}),
		),
		Button("Start task", VariantPrimary, "", ""),
	}
}

// Daemon is a daemon as the GUI shows it. Quota is its latest reading,
// taken at QuotaAt; nil when it has reported none. InUse of its Slots
// are held by tasks. LostSince is when the server declared it lost; zero
// while it is not. Refused is the reason the server last refused its
// command stream, at RefusedAt, with RefusedMessage; empty when none was
// refused since one opened.
type Daemon struct {
	ID        string
	Harness   string
	LastSeen  time.Time
	Connected bool
	LostSince time.Time
	Refused   string
	RefusedAt time.Time
	// RefusedMessage says why in words.
	RefusedMessage string
	Slots          int
	InUse          int
	Quota          *protocol.QuotaObserved
	QuotaAt        time.Time
	// Labels are its facts and the owner's labels, merged.
	Labels map[string]string
	// Login and Account are its login and account facts, empty when it
	// reports none.
	Login, Account string
}

// DaemonColumns head a Table of DaemonRows.
var DaemonColumns = []string{"Daemon", "Harness", "Last seen", "Connected", "Login", "Slots", "Labels", "Quota"}

// LoginNeeded flags a daemon whose harness reports that it is not
// logged in; the link leads to its page, where the owner logs it in.
func LoginNeeded(id string) html.Node {
	return html.El("a", attrs("href", daemonURL(id)+"#login", "class", "badge login-needed"), html.Text("login needed"))
}

// loginCell says whether daemon is logged in, and to which account.
func loginCell(daemon Daemon) html.Node {
	switch daemon.Login {
	case protocol.LoginNo:
		return LoginNeeded(daemon.ID)
	case protocol.LoginYes:
		if daemon.Account == "" {
			return html.Text("yes")
		}
		return html.Fragment(html.Text("yes, as "), html.El("code", nil, html.Text(daemon.Account)))
	}
	return html.El("span", attrs("class", "empty"), html.Text("not reported"))
}

// DaemonRow is a daemon in the daemon list.
func DaemonRow(daemon Daemon) html.Node {
	quota := html.Text("no reading yet")
	if daemon.Quota != nil {
		quota = QuotaReadout(*daemon.Quota, daemon.QuotaAt)
	}
	return html.El("tr", nil,
		cell(link(daemonURL(daemon.ID), daemon.ID)),
		cell(html.Text(daemon.Harness)),
		cell(timestamp(daemon.LastSeen)),
		cell(connection(daemon)),
		cell(loginCell(daemon)),
		cell(html.Text(fmt.Sprintf("%d of %d in use", daemon.InUse, daemon.Slots))),
		cell(LabelList(daemon.Labels), html.Text(" "), link(daemonURL(daemon.ID), "edit")),
		cell(quota),
	)
}

// connection says whether daemon is connected, and since when it is lost
// if it is.
func connection(daemon Daemon) html.Node {
	var refused html.Node = html.Fragment()
	if daemon.Refused != "" {
		refused = html.Fragment(html.Text(" "),
			html.El("span", attrs("class", "badge refused", "title", daemon.RefusedMessage), html.Text("stream refused: "+daemon.Refused)),
			html.Text(" at "), timestamp(daemon.RefusedAt))
	}
	if !daemon.Connected && !daemon.LostSince.IsZero() {
		return html.Fragment(html.Text("no, lost since "), timestamp(daemon.LostSince), refused)
	}
	return html.Fragment(html.Text(yesNo(daemon.Connected)), refused)
}

// BudgetReading is one budget's newest quota reading: Name says whose
// budget it is, Quota was taken at At, and is Age old.
type BudgetReading struct {
	Name  string
	Quota protocol.QuotaObserved
	At    time.Time
	Age   time.Duration
}

// Budget shows each budget's newest quota reading as the scheduler uses
// it, and the five-hour utilization that filler and low-priority turns
// must stay below. Each account has a budget of its own, as has each
// daemon that reports no account.
func Budget(readings []BudgetReading, fillerBelow, lowBelow float64) html.Node {
	thresholds := html.El("p", attrs("class", "notice"), html.Text(fmt.Sprintf(
		"Each account has its own budget, as has each daemon that reports none; a turn is checked against the budget of the daemon it goes to. "+
			"Filler runs while the five-hour window is below %s used, low priority below %s; "+
			"a rejected reading holds every turn until its window resets.", percent(fillerBelow), percent(lowBelow))))
	if len(readings) == 0 {
		return html.Fragment(html.El("p", attrs("class", "empty"), html.Text("No reading yet, so filler waits for one.")), thresholds)
	}
	items := make([]html.Node, len(readings))
	for idx, reading := range readings {
		items[idx] = html.El("li", nil,
			html.El("strong", nil, html.Text(reading.Name)),
			QuotaReadout(reading.Quota, reading.At),
			html.El("p", attrs("class", "notice"), html.Text("Taken "+ago(reading.Age)+" ago, by the newest turn on this budget.")))
	}
	return html.Fragment(html.El("ul", attrs("class", "budgets"), items...), thresholds)
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

// taskLinks links each of the tasks ids, separated by commas.
func taskLinks(ids []string) html.Node {
	links := make([]html.Node, 0, 2*len(ids))
	for idx, id := range ids {
		if idx > 0 {
			links = append(links, html.Text(", "))
		}
		links = append(links, link(taskURL(id), id))
	}
	return html.Fragment(links...)
}

// ContextSize words a session's context of tokens, out of the model's
// context window when that is known.
func ContextSize(tokens, window int64) string {
	if window <= 0 {
		return groupDigits(tokens) + " tokens"
	}
	return groupDigits(tokens) + " of " + groupDigits(window) + " tokens"
}

// groupDigits writes n with its digits grouped in threes by commas.
func groupDigits(n int64) string {
	digits := strconv.FormatInt(n, 10)
	var out strings.Builder
	for idx, digit := range digits {
		if idx > 0 && (len(digits)-idx)%3 == 0 && digits[idx-1] != '-' {
			out.WriteByte(',')
		}
		out.WriteRune(digit)
	}
	return out.String()
}

// ContinueForm starts a new task that continues the task taskID, with
// its final reply as the new task's prompt.
func ContinueForm(taskID string) html.Node {
	return html.El("div", attrs("class", "continue"), PlainForm(taskURL(taskID)+"/continue", "",
		Button("Continue in a new task", VariantPlain, "", "")))
}
