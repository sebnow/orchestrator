package component

import (
	"net/url"
	"strings"

	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// ProjectsURL lists the projects and has the form that creates one.
const ProjectsURL = "/projects"

func projectURL(id string) string { return ProjectsURL + "/" + url.PathEscape(id) }

// ProjectInput is a project as the owner entered it in ProjectForm. ID
// is empty for a project not yet created.
type ProjectInput struct {
	ID, Name, Instructions, Repo, Ref, DefaultAgent string
}

// ProjectColumns head a Table of ProjectRows.
var ProjectColumns = []string{"Project", "Repository", "Default agent", "Instructions"}

// ProjectRow is a project in the project list, linking to its page.
func ProjectRow(project ProjectInput) html.Node {
	var instructions html.Node
	if strings.TrimSpace(project.Instructions) != "" {
		instructions = html.Text(excerpt(project.Instructions))
	}
	return html.El("tr", nil,
		cell(link(projectURL(project.ID), project.Name)),
		cell(repository(project.Repo, project.Ref)),
		cell(agentLink(project.DefaultAgent)),
		cell(instructions),
	)
}

// repository is a repository and its ref, or says there is none.
func repository(repo, ref string) html.Node {
	if repo == "" {
		return html.El("span", attrs("class", "empty"), html.Text("none: an empty directory"))
	}
	return html.Fragment(html.El("code", nil, html.Text(repo)), html.Text(" at "), html.El("code", nil, html.Text(ref)))
}

// ProjectForm creates a project, or, when input.ID is set, updates it.
// agents are the agents it may name as its default. problem, when set,
// says why the last submission was refused.
func ProjectForm(input ProjectInput, agents []string, problem string) html.Node {
	action, submit := ProjectsURL, "Create project"
	if input.ID != "" {
		action, submit = projectURL(input.ID), "Save project"
	}
	options := []Option{{Value: "", Label: "None"}}
	for _, agent := range agents {
		options = append(options, Option{Value: agent, Label: agent})
	}
	return PlainForm(action, problem,
		Field(FieldSpec{Name: "name", Label: "Name", Value: input.Name, Required: true}),
		Field(FieldSpec{Kind: FieldTextarea, Name: "instructions", Label: "Instructions, added to the system prompt of every task in the project after its agent's", Value: input.Instructions, Rows: 12}),
		Field(FieldSpec{Name: "repo", Label: "Repository (https:// or ssh:// URL, or ssh address such as git@host:path)", Value: input.Repo, Placeholder: "none: each task names its own, or an empty directory"}),
		Field(FieldSpec{Name: "ref", Label: "Ref the tasks start from", Value: input.Ref}),
		Field(FieldSpec{Kind: FieldSelect, Name: "default_agent", Label: "Default agent, for tasks that name none", Value: input.DefaultAgent, Options: options}),
		Button(submit, VariantPrimary, "", ""),
	)
}

// DeleteProjectForm deletes the project id.
func DeleteProjectForm(id string) html.Node {
	return PlainForm(projectURL(id)+"/delete", "", Button("Delete project", VariantDanger, "", ""))
}

// RootTask is a task the owner started in a project, as the project's
// page lists it.
type RootTask struct {
	ID, Title, Agent, State string
	CostUSD                 float64
	Branch                  *transcript.BranchPushed
}

// RootTaskColumns head a Table of RootTaskRows.
var RootTaskColumns = []string{"State", "Task", "Agent", "Cost", "Branch"}

// RootTaskRow is a root task in its project's list, linking to its page.
func RootTaskRow(task RootTask) html.Node {
	return html.El("tr", nil,
		cell(StateBadge(task.State)),
		cell(link(taskURL(task.ID), excerpt(task.Title))),
		cell(agentLink(task.Agent)),
		cell(html.Text(cost(task.CostUSD))),
		cell(childBranch(task.Branch)),
	)
}

// projectLink links the project id by name, or by its id when the name
// is not known; it is empty for none.
func projectLink(id, name string) html.Node {
	if id == "" {
		return nil
	}
	if name == "" {
		name = id
	}
	return link(projectURL(id), name)
}
