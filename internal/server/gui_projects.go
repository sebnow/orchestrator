package server

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/transcript"
)

// routeProjectsGUI serves the pages that list, create, edit and delete
// projects, and start tasks in them. Their forms are plain: each POST
// answers with a redirect, or with the page and the form's problem.
func (s *Server) routeProjectsGUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /projects", s.getProjectsPage)
	mux.HandleFunc("POST /projects", s.postProjectForm)
	mux.HandleFunc("GET /projects/{project}", s.getProjectPage)
	mux.HandleFunc("POST /projects/{project}", s.postProjectForm)
	mux.HandleFunc("POST /projects/{project}/delete", s.postDeleteProjectForm)
}

func guiProject(p Project) component.ProjectInput {
	return component.ProjectInput{ID: p.ID, Name: p.Name, Instructions: p.Instructions, Repo: p.Repo, Ref: p.Ref, DefaultAgent: p.DefaultAgent}
}

// agentNames returns every agent's name.
func (s *Server) agentNames(ctx context.Context) ([]string, error) {
	agents, err := s.store.agents(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(agents))
	for idx, a := range agents {
		names[idx] = a.Name
	}
	return names, nil
}

func (s *Server) getProjectsPage(w http.ResponseWriter, r *http.Request) {
	s.writeProjectsPage(w, r, http.StatusOK, component.ProjectInput{}, "")
}

// writeProjectsPage writes the project list with input in the form that
// creates a project, and problem, when set, saying why it was refused.
func (s *Server) writeProjectsPage(w http.ResponseWriter, r *http.Request, status int, input component.ProjectInput, problem string) {
	projects, err := s.store.projects(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	agents, err := s.agentNames(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	rows := make([]html.Node, len(projects))
	for idx, p := range projects {
		rows[idx] = component.ProjectRow(guiProject(p))
	}
	s.writeHTML(w, status, component.Page("Projects",
		component.Section("Projects", component.Table(component.ProjectColumns, "No projects yet.", rows...)),
		component.Section("New project", component.ProjectForm(input, agents, problem)),
	))
}

func (s *Server) getProjectPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.project(r.Context(), r.PathValue("project"))
	if errors.Is(err, errUnknownProject) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeProjectPage(w, r, http.StatusOK, guiProject(p), "")
}

// writeProjectPage writes the page of the project input names: the form
// that edits it with input and problem, its root tasks, and the form that
// starts a task in it as it is stored.
func (s *Server) writeProjectPage(w http.ResponseWriter, r *http.Request, status int, input component.ProjectInput, problem string) {
	ctx := r.Context()
	stored, err := s.store.project(ctx, input.ID)
	if errors.Is(err, errUnknownProject) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	roots, err := s.rootTasks(ctx, input.ID)
	if err != nil {
		s.internalError(w, err)
		return
	}
	daemons, err := s.store.daemons(ctx)
	if err != nil {
		s.internalError(w, err)
		return
	}
	ids := make([]string, len(daemons))
	for idx, daemon := range daemons {
		ids[idx] = string(daemon.ID)
	}
	choices, err := s.taskChoices(ctx, ids)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeHTML(w, status, component.Page("Project "+stored.Name,
		component.Section("Project "+stored.Name, component.ProjectForm(input, choices.Agents, problem)),
		component.Section("Tasks", roots),
		component.Section("New task", component.ProjectTaskForm(component.NewTask{Agent: stored.DefaultAgent}, choices, guiProject(stored))),
		component.Section("Delete", component.DeleteProjectForm(stored.ID)),
	))
}

// rootTasks lists the tasks the owner started in the project with id,
// newest first.
func (s *Server) rootTasks(ctx context.Context, id string) (html.Node, error) {
	summaries, err := s.store.tasks(ctx)
	if err != nil {
		return nil, err
	}
	prompts, err := s.store.taskPrompts(ctx)
	if err != nil {
		return nil, err
	}
	var rows []html.Node
	for _, summary := range slices.Backward(summaries) {
		if summary.Project != id || summary.ParentID != nil {
			continue
		}
		title := prompts[summary.ID]
		if summary.Purpose != "" {
			title = summary.Purpose
		}
		row := component.RootTask{ID: string(summary.ID), Title: title, Agent: summary.Agent, State: string(summary.State), CostUSD: summary.CostUSD}
		if summary.Branch != nil {
			pushed := transcript.BranchPushed(*summary.Branch)
			row.Branch = &pushed
		}
		rows = append(rows, component.RootTaskRow(row))
	}
	return component.Table(component.RootTaskColumns, "No tasks in this project yet.", rows...), nil
}

// postProjectForm creates a project, or updates the one the path names,
// and returns the owner to its page.
func (s *Server) postProjectForm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxOwnerRequestBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "read form: "+err.Error(), http.StatusBadRequest)
		return
	}
	input := component.ProjectInput{
		ID:           r.PathValue("project"),
		Name:         r.PostForm.Get("name"),
		Instructions: r.PostForm.Get("instructions"),
		Repo:         r.PostForm.Get("repo"),
		Ref:          r.PostForm.Get("ref"),
		DefaultAgent: r.PostForm.Get("default_agent"),
	}
	editing := input.ID != ""
	refuse := func(status int, problem string) {
		if editing {
			s.writeProjectPage(w, r, status, input, problem)
		} else {
			s.writeProjectsPage(w, r, status, input, problem)
		}
	}
	p := Project{ID: input.ID, Name: input.Name, Instructions: input.Instructions, Repo: input.Repo, Ref: input.Ref, DefaultAgent: input.DefaultAgent}
	if err := p.normalise(); err != nil {
		refuse(http.StatusUnprocessableEntity, "The project was not saved: "+err.Error()+".")
		return
	}
	var err error
	if editing {
		p, err = s.store.updateProject(r.Context(), p)
	} else {
		p, err = s.store.createProject(r.Context(), p)
	}
	switch {
	case errors.Is(err, errUnknownProject):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errProjectExists):
		refuse(http.StatusConflict, "A project named "+p.Name+" exists already; edit it, or choose another name.")
	case errors.Is(err, errUnknownAgent):
		refuse(http.StatusUnprocessableEntity, "There is no agent "+p.DefaultAgent+".")
	case err != nil:
		s.internalError(w, err)
	default:
		redirect(w, r, component.ProjectsURL+"/"+p.ID)
	}
}

// postDeleteProjectForm deletes the project the path names and returns
// the owner to the project list, unless a task belongs to the project.
func (s *Server) postDeleteProjectForm(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("project")
	err := s.store.deleteProject(r.Context(), id)
	switch {
	case errors.Is(err, errUnknownProject):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errProjectInUse):
		p, err := s.store.project(r.Context(), id)
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.writeProjectPage(w, r, http.StatusConflict, guiProject(p), "Tasks belong to this project, so it cannot be deleted. Edit it instead.")
	case err != nil:
		s.internalError(w, err)
	default:
		redirect(w, r, component.ProjectsURL)
	}
}
