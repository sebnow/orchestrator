package server

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/component"
	"github.com/sebnow/orchestrator/internal/html"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// routeAgentsGUI serves the pages that list, create, edit and delete
// agents. Their forms are plain: each POST answers with a redirect, or
// with the page and the form's problem.
func (s *Server) routeAgentsGUI(mux *http.ServeMux) {
	mux.HandleFunc("GET /agents", s.getAgentsPage)
	mux.HandleFunc("POST /agents", s.postAgentForm)
	mux.HandleFunc("GET /agents/{agent}", s.getAgentPage)
	mux.HandleFunc("POST /agents/{agent}", s.postAgentForm)
	mux.HandleFunc("POST /agents/{agent}/delete", s.postDeleteAgentForm)
}

func agentInput(a Agent) component.AgentInput {
	input := component.AgentInput{
		Name: a.Name, Description: a.Description, SystemPrompt: a.SystemPrompt, Model: a.Model,
		Tools: a.Tools, Priority: string(a.Priority), Requires: a.Requires.String(),
	}
	if a.PauseLimits != nil {
		input.Acknowledge, input.Cleanup = a.PauseLimits.Acknowledge.String(), a.PauseLimits.Cleanup.String()
	}
	if a.Filler {
		input.Filler = "on"
	}
	return input
}

func (s *Server) getAgentsPage(w http.ResponseWriter, r *http.Request) {
	s.writeAgentsPage(w, r, http.StatusOK, component.AgentInput{Priority: string(PriorityNormal)}, "")
}

// writeAgentsPage writes the agent list with input in the form that
// creates an agent, and problem, when set, saying why it was refused.
func (s *Server) writeAgentsPage(w http.ResponseWriter, r *http.Request, status int, input component.AgentInput, problem string) {
	agents, err := s.store.agents(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	rows := make([]html.Node, len(agents))
	for idx, a := range agents {
		rows[idx] = component.AgentRow(component.AgentSummary{
			Name: a.Name, Description: a.Description, Model: a.Model, Tools: a.Tools, Requires: a.Requires.String(),
		})
	}
	s.writeHTML(w, status, component.Page("Agents",
		component.Section("Agents", component.Table(component.AgentColumns, "No agents yet.", rows...)),
		component.Section("New agent", component.AgentForm(input, false, protocol.AgentTools, problem)),
	))
}

func (s *Server) getAgentPage(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.agent(r.Context(), r.PathValue("agent"))
	if errors.Is(err, errUnknownAgent) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.writeAgentPage(w, http.StatusOK, agentInput(a), "")
}

// writeAgentPage writes the page that edits the agent input names.
func (s *Server) writeAgentPage(w http.ResponseWriter, status int, input component.AgentInput, problem string) {
	s.writeHTML(w, status, component.Page("Agent "+input.Name,
		component.Section("Agent "+input.Name, component.AgentForm(input, true, protocol.AgentTools, problem)),
		component.Section("Delete", component.DeleteAgentForm(input.Name)),
	))
}

// postAgentForm creates an agent, or updates the one the path names, and
// returns the owner to its page.
func (s *Server) postAgentForm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxOwnerRequestBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "read form: "+err.Error(), http.StatusBadRequest)
		return
	}
	editing := r.PathValue("agent") != ""
	input := component.AgentInput{
		Name:         strings.TrimSpace(r.PostForm.Get("name")),
		Description:  r.PostForm.Get("description"),
		SystemPrompt: r.PostForm.Get("system_prompt"),
		Model:        strings.TrimSpace(r.PostForm.Get("model")),
		Tools:        r.PostForm["tools"],
		Acknowledge:  strings.TrimSpace(r.PostForm.Get("acknowledge")),
		Cleanup:      strings.TrimSpace(r.PostForm.Get("cleanup")),
		Priority:     r.PostForm.Get("priority"),
		Filler:       r.PostForm.Get("filler"),
		Requires:     r.PostForm.Get("requires"),
	}
	if editing {
		input.Name = r.PathValue("agent")
	}
	refuse := func(status int, problem string) {
		if editing {
			s.writeAgentPage(w, status, input, problem)
		} else {
			s.writeAgentsPage(w, r, status, input, problem)
		}
	}
	a, problem := agentFromInput(input)
	if problem != "" {
		refuse(http.StatusUnprocessableEntity, problem)
		return
	}
	var err error
	if editing {
		err = s.store.updateAgent(r.Context(), a)
	} else {
		err = s.store.createAgent(r.Context(), a)
	}
	switch {
	case errors.Is(err, errUnknownAgent):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errAgentExists):
		refuse(http.StatusConflict, "An agent named "+a.Name+" exists already; edit it, or choose another name.")
	case err != nil:
		s.internalError(w, err)
	default:
		redirect(w, r, "/agents/"+a.Name)
	}
}

// agentFromInput reads an agent from the form, or returns why it is
// refused, as text for the owner.
func agentFromInput(input component.AgentInput) (Agent, string) {
	a := Agent{
		Name: input.Name, Description: input.Description, SystemPrompt: input.SystemPrompt, Model: input.Model,
		Tools: input.Tools, Priority: Priority(input.Priority), Filler: input.Filler != "",
	}
	switch {
	case input.Acknowledge == "" && input.Cleanup == "":
	case input.Acknowledge == "" || input.Cleanup == "":
		return Agent{}, "Give both pause limits, or neither for the task defaults."
	default:
		acknowledge, err := time.ParseDuration(input.Acknowledge)
		if err != nil {
			return Agent{}, "The acknowledge limit is not a duration such as 1m or 90s."
		}
		cleanup, err := time.ParseDuration(input.Cleanup)
		if err != nil {
			return Agent{}, "The cleanup limit is not a duration such as 5m."
		}
		a.PauseLimits = &protocol.PauseLimits{Acknowledge: acknowledge, Cleanup: cleanup}
	}
	requires, err := ParseLabels(input.Requires)
	if err != nil {
		return Agent{}, "Requires: " + err.Error() + "."
	}
	a.Requires = requires
	if err := a.normalise(); err != nil {
		return Agent{}, "The agent was not saved: " + err.Error() + "."
	}
	return a, ""
}

// postDeleteAgentForm deletes the agent the path names and returns the
// owner to the agent list, unless a task names the agent.
func (s *Server) postDeleteAgentForm(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("agent")
	err := s.store.deleteAgent(r.Context(), name)
	switch {
	case errors.Is(err, errUnknownAgent):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errAgentInUse):
		a, err := s.store.agent(r.Context(), name)
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.writeAgentPage(w, http.StatusConflict, agentInput(a), "Tasks were started as this agent, so it cannot be deleted. Edit it instead.")
	case err != nil:
		s.internalError(w, err)
	default:
		redirect(w, r, component.AgentsURL)
	}
}
