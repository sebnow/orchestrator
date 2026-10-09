package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// Agent is an agent definition: what a task started as the agent gets
// (docs/adr/2026-10-09-agents-and-placement.md). Model, PauseLimits and
// Requires are what such a task takes when its request leaves them out;
// a nil PauseLimits leaves the task defaults. Tools are the gateway
// tools its tasks may call, of protocol.AgentTools; it is never nil.
type Agent struct {
	Name         string                `json:"name"`
	Description  string                `json:"description"`
	SystemPrompt string                `json:"system_prompt"`
	Model        string                `json:"model,omitempty"`
	Tools        []string              `json:"tools"`
	PauseLimits  *protocol.PauseLimits `json:"pause_limits,omitempty"`
	Priority     Priority              `json:"priority"`
	Filler       bool                  `json:"filler"`
	Requires     Labels                `json:"requires"`
}

var (
	errUnknownAgent = errors.New("unknown agent")
	errAgentExists  = errors.New("an agent with that name exists")
	// errAgentInUse refuses to delete an agent a task names.
	errAgentInUse = errors.New("agent is named by a task")
)

// normalise checks a and fills in what it leaves out: no tools, normal
// priority, no requirements.
func (a *Agent) normalise() error {
	if _, err := protocol.ParseTaskID(a.Name); err != nil {
		return fmt.Errorf("name %q must be up to 128 letters, digits, '.', '_' and '-', other than . and ..", a.Name)
	}
	if a.Tools == nil {
		a.Tools = []string{}
	}
	if err := validateTools(a.Tools); err != nil {
		return err
	}
	priority, err := ParsePriority(string(a.Priority))
	if err != nil {
		return err
	}
	a.Priority = priority
	if a.PauseLimits != nil && (a.PauseLimits.Acknowledge <= 0 || a.PauseLimits.Cleanup <= 0) {
		return errors.New("pause_limits.acknowledge and pause_limits.cleanup must be positive")
	}
	if a.Requires == nil {
		a.Requires = Labels{}
	}
	if err := a.Requires.Validate(); err != nil {
		return fmt.Errorf("requires: %w", err)
	}
	return nil
}

// agentColumns are an agent's columns, in the order scanAgent reads them.
const agentColumns = `name, description, system_prompt, model, tools, pause_acknowledge_ns, pause_cleanup_ns, priority, filler, requires`

func scanAgent(row interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	var tools, priority, requires string
	var acknowledge, cleanup sql.NullInt64
	if err := row.Scan(&a.Name, &a.Description, &a.SystemPrompt, &a.Model, &tools, &acknowledge, &cleanup, &priority, &a.Filler, &requires); err != nil {
		return Agent{}, err
	}
	if err := json.Unmarshal([]byte(tools), &a.Tools); err != nil {
		return Agent{}, fmt.Errorf("agent %q tools: %w", a.Name, err)
	}
	if acknowledge.Valid && cleanup.Valid {
		a.PauseLimits = &protocol.PauseLimits{Acknowledge: time.Duration(acknowledge.Int64), Cleanup: time.Duration(cleanup.Int64)}
	}
	a.Priority = Priority(priority)
	var err error
	if a.Requires, err = decodeLabels(requires); err != nil {
		return Agent{}, fmt.Errorf("agent %q requires: %w", a.Name, err)
	}
	return a, nil
}

// agentValues are a's values for agentColumns.
func agentValues(a Agent) []any {
	tools, err := json.Marshal(a.Tools)
	if err != nil {
		panic(err)
	}
	var acknowledge, cleanup any
	if a.PauseLimits != nil {
		acknowledge, cleanup = int64(a.PauseLimits.Acknowledge), int64(a.PauseLimits.Cleanup)
	}
	return []any{a.Name, a.Description, a.SystemPrompt, a.Model, string(tools), acknowledge, cleanup, string(a.Priority), a.Filler, encodeLabels(a.Requires)}
}

// agents returns every agent, by name.
func (s *Store) agents(ctx context.Context) ([]Agent, error) {
	return queryAgents(ctx, s.db)
}

func queryAgents(ctx context.Context, db interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}) ([]Agent, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+agentColumns+` FROM agents ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("read agents: %w", err)
	}
	defer rows.Close()
	agents := []Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, fmt.Errorf("read agents: %w", err)
		}
		agents = append(agents, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read agents: %w", err)
	}
	return agents, nil
}

// agent returns the agent named name, or errUnknownAgent.
func (s *Store) agent(ctx context.Context, name string) (Agent, error) {
	return queryAgent(ctx, s.db, name)
}

type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func queryAgent(ctx context.Context, db queryRower, name string) (Agent, error) {
	a, err := scanAgent(db.QueryRowContext(ctx, `SELECT `+agentColumns+` FROM agents WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, fmt.Errorf("%w: %q", errUnknownAgent, name)
	}
	if err != nil {
		return Agent{}, fmt.Errorf("read agent %q: %w", name, err)
	}
	return a, nil
}

// createAgent records a, which must be normalised, or returns
// errAgentExists.
func (s *Store) createAgent(ctx context.Context, a Agent) error {
	now := formatTime(time.Now().UTC())
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO agents (`+agentColumns+`, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO NOTHING`, append(agentValues(a), now, now)...)
	if err != nil {
		return fmt.Errorf("create agent %q: %w", a.Name, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return fmt.Errorf("create agent %q: %w", a.Name, err)
		}
		return fmt.Errorf("%w: %q", errAgentExists, a.Name)
	}
	return nil
}

// updateAgent replaces the agent named a.Name with a, which must be
// normalised, or returns errUnknownAgent.
func (s *Store) updateAgent(ctx context.Context, a Agent) error {
	values := agentValues(a)
	result, err := s.db.ExecContext(ctx, `
		UPDATE agents SET description = ?, system_prompt = ?, model = ?, tools = ?, pause_acknowledge_ns = ?, pause_cleanup_ns = ?,
			priority = ?, filler = ?, requires = ?, updated_at = ?
		WHERE name = ?`, append(values[1:], formatTime(time.Now().UTC()), a.Name)...)
	if err != nil {
		return fmt.Errorf("update agent %q: %w", a.Name, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return fmt.Errorf("update agent %q: %w", a.Name, err)
		}
		return fmt.Errorf("%w: %q", errUnknownAgent, a.Name)
	}
	return nil
}

// deleteAgent deletes the agent named name, unless a task names it, when
// it returns errAgentInUse, or it does not exist, when it returns
// errUnknownAgent.
func (s *Store) deleteAgent(ctx context.Context, name string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete agent %q: %w", name, err)
	}
	defer tx.Rollback()
	var tasks int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE agent = ?`, name).Scan(&tasks); err != nil {
		return fmt.Errorf("delete agent %q: %w", name, err)
	}
	if tasks > 0 {
		return fmt.Errorf("%w: %d tasks name %q", errAgentInUse, tasks, name)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM agents WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete agent %q: %w", name, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return fmt.Errorf("delete agent %q: %w", name, err)
		}
		return fmt.Errorf("%w: %q", errUnknownAgent, name)
	}
	return tx.Commit()
}

// agentsPrompt lists agents for a task that may spawn them, or returns ""
// when there are none.
func agentsPrompt(agents []Agent) string {
	if len(agents) == 0 {
		return ""
	}
	lines := []string{"You can start a child task as one of these agents by giving its name as spawn_task's agent:"}
	for _, a := range agents {
		line := "- " + a.Name
		if description := strings.Join(strings.Fields(a.Description), " "); description != "" {
			line += ": " + description
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// getAgents lists every agent, by name.
func (s *Server) getAgents(w http.ResponseWriter, r *http.Request) {
	agents, err := s.store.agents(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

// getAgent returns the agent the path names.
func (s *Server) getAgent(w http.ResponseWriter, r *http.Request) {
	a, err := s.store.agent(r.Context(), r.PathValue("agent"))
	if errors.Is(err, errUnknownAgent) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// decodeAgent reads and checks an agent from the request's body.
func decodeAgent(w http.ResponseWriter, r *http.Request) (Agent, bool) {
	var a Agent
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxOwnerRequestBytes), &a); err != nil {
		http.Error(w, "decode agent: "+err.Error(), http.StatusBadRequest)
		return Agent{}, false
	}
	if err := a.normalise(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return Agent{}, false
	}
	return a, true
}

// postAgent creates an agent and returns it with 201, or 409 when one
// has its name.
func (s *Server) postAgent(w http.ResponseWriter, r *http.Request) {
	a, ok := decodeAgent(w, r)
	if !ok {
		return
	}
	err := s.store.createAgent(r.Context(), a)
	if errors.Is(err, errAgentExists) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// putAgent replaces the agent the path names, whose name does not
// change, and returns it.
func (s *Server) putAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("agent")
	var a Agent
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxOwnerRequestBytes), &a); err != nil {
		http.Error(w, "decode agent: "+err.Error(), http.StatusBadRequest)
		return
	}
	if a.Name != "" && a.Name != name {
		http.Error(w, "an agent's name does not change; create an agent with the new name instead", http.StatusBadRequest)
		return
	}
	a.Name = name
	if err := a.normalise(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err := s.store.updateAgent(r.Context(), a)
	if errors.Is(err, errUnknownAgent) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// deleteAgentRequest deletes the agent the path names, answering 204,
// or 409 while a task names it.
func (s *Server) deleteAgentRequest(w http.ResponseWriter, r *http.Request) {
	err := s.store.deleteAgent(r.Context(), r.PathValue("agent"))
	switch {
	case errors.Is(err, errUnknownAgent):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errAgentInUse):
		http.Error(w, err.Error()+"; an agent that a task names cannot be deleted", http.StatusConflict)
	case err != nil:
		s.internalError(w, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
