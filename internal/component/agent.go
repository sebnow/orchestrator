package component

import (
	"net/url"
	"slices"
	"strings"

	"github.com/sebnow/orchestrator/internal/html"
)

// AgentsURL lists the agents and has the form that creates one.
const AgentsURL = "/agents"

func agentURL(name string) string { return AgentsURL + "/" + url.PathEscape(name) }

// AgentInput is an agent as the owner entered it in AgentForm. Tools
// holds the names of the ticked tools; Acknowledge and Cleanup are Go
// durations, both empty for the task defaults; Filler is "on" when
// ticked; Requires is key=value pairs.
type AgentInput struct {
	Name, Description, SystemPrompt, Model string
	Tools                                  []string
	Acknowledge, Cleanup                   string
	Priority                               string
	Filler                                 string
	Requires                               string
}

// AgentSummary is an agent as the agent list shows it.
type AgentSummary struct {
	Name, Description, Model string
	Tools                    []string
	Requires                 string
}

// AgentColumns head a Table of AgentRows.
var AgentColumns = []string{"Agent", "Description", "Tools", "Model", "Requires"}

// AgentRow is an agent in the agent list, linking to its page.
func AgentRow(agent AgentSummary) html.Node {
	var description html.Node
	if strings.TrimSpace(agent.Description) != "" {
		description = html.Text(excerpt(agent.Description))
	}
	tools := strings.Join(agent.Tools, ", ")
	if tools == "" {
		tools = "none"
	}
	return html.El("tr", nil,
		cell(link(agentURL(agent.Name), agent.Name)),
		cell(description),
		cell(html.Text(tools)),
		cell(html.Text(orNone(agent.Model))),
		cell(html.Text(orNone(agent.Requires))),
	)
}

// AgentForm creates an agent, or, when editing, updates the agent named
// input.Name, whose name it shows but does not change. tools are the
// gateway tools an agent may be allowed. problem, when set, says why the
// last submission was refused.
func AgentForm(input AgentInput, editing bool, tools []string, problem string) html.Node {
	action, submit := AgentsURL, "Create agent"
	name := Field(FieldSpec{Name: "name", Label: "Name (letters, digits, '.', '_' and '-')", Value: input.Name, Required: true})
	if editing {
		action, submit = agentURL(input.Name), "Save agent"
		name = nil
	}
	boxes := make([]html.Node, len(tools))
	for idx, tool := range tools {
		box := attrs("type", "checkbox", "name", "tools", "value", tool)
		if slices.Contains(input.Tools, tool) {
			box = append(box, html.Attr("checked", ""))
		}
		boxes[idx] = html.El("label", attrs("class", "checkbox"), html.El("input", box), html.Text(" "+tool))
	}
	priorities := make([]Option, len(Priorities))
	for idx, priority := range Priorities {
		priorities[idx] = Option{Value: priority, Label: priority}
	}
	return PlainForm(action, problem,
		name,
		Field(FieldSpec{Kind: FieldTextarea, Name: "description", Label: "Description, shown to agents that may spawn it", Value: input.Description}),
		Field(FieldSpec{Kind: FieldTextarea, Name: "system_prompt", Label: "System prompt", Value: input.SystemPrompt, Rows: 16}),
		Field(FieldSpec{Name: "model", Label: "Model", Value: input.Model, Placeholder: "none: the task's or the server's default"}),
		html.El("fieldset", nil, html.El("legend", nil, html.Text("Tools it may call besides the permission and pause tools")), html.Fragment(boxes...)),
		Field(FieldSpec{Kind: FieldSelect, Name: "priority", Label: "Priority", Value: input.Priority, Options: priorities}),
		Field(FieldSpec{Kind: FieldCheckbox, Name: "filler", Label: "Filler: runs only on spare budget, and yields to other work", Value: input.Filler}),
		Field(FieldSpec{Name: "requires", Label: "Requires labels (key=value, separated by commas)", Value: input.Requires, Placeholder: "none: any daemon"}),
		Details("Pause limits",
			Field(FieldSpec{Name: "acknowledge", Label: "Acknowledge within", Value: input.Acknowledge, Placeholder: "task default"}),
			Field(FieldSpec{Name: "cleanup", Label: "Clean up within", Value: input.Cleanup, Placeholder: "task default"}),
		),
		Button(submit, VariantPrimary, "", ""),
	)
}

// DeleteAgentForm deletes the agent name.
func DeleteAgentForm(name string) html.Node {
	return PlainForm(agentURL(name)+"/delete", "", Button("Delete agent", VariantDanger, "", ""))
}
