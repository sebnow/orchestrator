package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// Project is a reusable container the owner defines for tasks
// (docs/adr/2026-10-10-projects-and-lineage.md): instructions added to
// the system prompt of every task in it, a repository its tasks work in,
// and the agent its tasks are started as when they name none. Repo and
// Ref are both empty when it has no repository, and DefaultAgent is empty
// for none. The server gives it its ID, which does not change; its name
// may.
type Project struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Instructions string    `json:"instructions"`
	Repo         string    `json:"repo,omitempty"`
	Ref          string    `json:"ref,omitempty"`
	DefaultAgent string    `json:"default_agent,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// maxProjectNameRunes bounds a project's name, which the GUI shows in
// lists.
const maxProjectNameRunes = 128

var (
	errUnknownProject = errors.New("unknown project")
	errProjectExists  = errors.New("a project with that name exists")
	// errProjectInUse refuses to delete a project a task belongs to.
	errProjectInUse = errors.New("project has tasks")
)

// normalise checks p and trims its name, repository and ref.
func (p *Project) normalise() error {
	p.Name = strings.TrimSpace(p.Name)
	p.Repo, p.Ref, p.DefaultAgent = strings.TrimSpace(p.Repo), strings.TrimSpace(p.Ref), strings.TrimSpace(p.DefaultAgent)
	switch {
	case p.Name == "":
		return errors.New("name is required")
	case strings.ContainsAny(p.Name, "\r\n"):
		return errors.New("name must be one line")
	case utf8.RuneCountInString(p.Name) > maxProjectNameRunes:
		return fmt.Errorf("name must be at most %d characters", maxProjectNameRunes)
	case (p.Repo == "") != (p.Ref == ""):
		return errors.New("repo and ref go together: give both, or neither for no repository")
	}
	return nil
}

// Workspace is the repository the project's tasks work in, or nil when
// it has none.
func (p Project) Workspace() *protocol.Workspace {
	if p.Repo == "" {
		return nil
	}
	return &protocol.Workspace{Repo: p.Repo, Ref: p.Ref}
}

const projectColumns = `id, name, instructions, repo, ref, default_agent, created_at, updated_at`

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var p Project
	var repo, ref, agent sql.NullString
	var created, updated string
	if err := row.Scan(&p.ID, &p.Name, &p.Instructions, &repo, &ref, &agent, &created, &updated); err != nil {
		return Project{}, err
	}
	p.Repo, p.Ref, p.DefaultAgent = repo.String, ref.String, agent.String
	var err error
	if p.CreatedAt, err = parseTime(created); err != nil {
		return Project{}, fmt.Errorf("project %q created_at: %w", p.ID, err)
	}
	if p.UpdatedAt, err = parseTime(updated); err != nil {
		return Project{}, fmt.Errorf("project %q updated_at: %w", p.ID, err)
	}
	return p, nil
}

// projects returns every project, by name.
func (s *Store) projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("read projects: %w", err)
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("read projects: %w", err)
		}
		projects = append(projects, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read projects: %w", err)
	}
	return projects, nil
}

// project returns the project with id, or errUnknownProject.
func (s *Store) project(ctx context.Context, id string) (Project, error) {
	return queryProject(ctx, s.db, id)
}

func queryProject(ctx context.Context, db queryRower, id string) (Project, error) {
	p, err := scanProject(db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("%w: %q", errUnknownProject, id)
	}
	if err != nil {
		return Project{}, fmt.Errorf("read project %q: %w", id, err)
	}
	return p, nil
}

// checkProject refuses p, which must be normalised, when another project
// than p.ID has its name, with errProjectExists, or when its default
// agent does not exist, with errUnknownAgent.
func checkProject(ctx context.Context, tx *sql.Tx, p Project) error {
	var taken bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE name = ? AND id <> ?)`, p.Name, p.ID).Scan(&taken); err != nil {
		return fmt.Errorf("look up project %q: %w", p.Name, err)
	}
	if taken {
		return fmt.Errorf("%w: %q", errProjectExists, p.Name)
	}
	if p.DefaultAgent != "" {
		if _, err := queryAgent(ctx, tx, p.DefaultAgent); err != nil {
			return err
		}
	}
	return nil
}

// createProject records p, which must be normalised, under a new id, and
// returns it as recorded.
func (s *Store) createProject(ctx context.Context, p Project) (Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	defer tx.Rollback()
	// rand.Text uses only letters and digits, so the id is safe in a path.
	p.ID = rand.Text()
	if err := checkProject(ctx, tx, p); err != nil {
		return Project{}, err
	}
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	_, err = tx.ExecContext(ctx, `INSERT INTO projects (`+projectColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Name, p.Instructions, nullable(p.Repo), nullable(p.Ref), nullable(p.DefaultAgent), formatTime(now), formatTime(now))
	if err != nil {
		return Project{}, fmt.Errorf("create project %q: %w", p.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("create project %q: %w", p.Name, err)
	}
	return p, nil
}

// updateProject replaces the project with p.ID by p, which must be
// normalised, and returns it as recorded, or returns errUnknownProject.
func (s *Store) updateProject(ctx context.Context, p Project) (Project, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Project{}, fmt.Errorf("update project %q: %w", p.ID, err)
	}
	defer tx.Rollback()
	old, err := queryProject(ctx, tx, p.ID)
	if err != nil {
		return Project{}, err
	}
	if err := checkProject(ctx, tx, p); err != nil {
		return Project{}, err
	}
	p.CreatedAt, p.UpdatedAt = old.CreatedAt, time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
		UPDATE projects SET name = ?, instructions = ?, repo = ?, ref = ?, default_agent = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.Instructions, nullable(p.Repo), nullable(p.Ref), nullable(p.DefaultAgent), formatTime(p.UpdatedAt), p.ID)
	if err != nil {
		return Project{}, fmt.Errorf("update project %q: %w", p.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return Project{}, fmt.Errorf("update project %q: %w", p.ID, err)
	}
	return p, nil
}

// deleteProject deletes the project with id, unless a task belongs to
// it, when it returns errProjectInUse, or it does not exist, when it
// returns errUnknownProject.
func (s *Store) deleteProject(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("delete project %q: %w", id, err)
	}
	defer tx.Rollback()
	var tasks int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM tasks WHERE project = ?`, id).Scan(&tasks); err != nil {
		return fmt.Errorf("delete project %q: %w", id, err)
	}
	if tasks > 0 {
		return fmt.Errorf("%w: %d tasks belong to project %q", errProjectInUse, tasks, id)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete project %q: %w", id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		if err != nil {
			return fmt.Errorf("delete project %q: %w", id, err)
		}
		return fmt.Errorf("%w: %q", errUnknownProject, id)
	}
	return tx.Commit()
}

// projectInput is a project as the owner API takes it: its id and times
// are the server's.
type projectInput struct {
	Name         string `json:"name"`
	Instructions string `json:"instructions"`
	Repo         string `json:"repo"`
	Ref          string `json:"ref"`
	DefaultAgent string `json:"default_agent"`
}

// decodeProject reads and checks a project from the request's body.
func decodeProject(w http.ResponseWriter, r *http.Request) (Project, bool) {
	var input projectInput
	if err := decodeStrict(http.MaxBytesReader(w, r.Body, maxOwnerRequestBytes), &input); err != nil {
		http.Error(w, "decode project: "+err.Error(), http.StatusBadRequest)
		return Project{}, false
	}
	p := Project{Name: input.Name, Instructions: input.Instructions, Repo: input.Repo, Ref: input.Ref, DefaultAgent: input.DefaultAgent}
	if err := p.normalise(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return Project{}, false
	}
	return p, true
}

// writeProjectError answers a store error from saving a project, or
// reports false when err is nil.
func (s *Server) writeProjectError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errUnknownProject):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errProjectExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, errUnknownAgent):
		http.Error(w, "default_agent: "+err.Error(), http.StatusUnprocessableEntity)
	default:
		s.internalError(w, err)
	}
	return true
}

// getProjects lists every project, by name.
func (s *Server) getProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.store.projects(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, projects)
}

// getProject returns the project the path names.
func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.project(r.Context(), r.PathValue("project"))
	if s.writeProjectError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// postProject creates a project and returns it with 201, or 409 when one
// has its name.
func (s *Server) postProject(w http.ResponseWriter, r *http.Request) {
	p, ok := decodeProject(w, r)
	if !ok {
		return
	}
	created, err := s.store.createProject(r.Context(), p)
	if s.writeProjectError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// putProject replaces the project the path names, keeping its id, and
// returns it.
func (s *Server) putProject(w http.ResponseWriter, r *http.Request) {
	p, ok := decodeProject(w, r)
	if !ok {
		return
	}
	p.ID = r.PathValue("project")
	updated, err := s.store.updateProject(r.Context(), p)
	if s.writeProjectError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

// deleteProjectRequest deletes the project the path names, answering
// 204, or 409 while a task belongs to it.
func (s *Server) deleteProjectRequest(w http.ResponseWriter, r *http.Request) {
	err := s.store.deleteProject(r.Context(), r.PathValue("project"))
	switch {
	case errors.Is(err, errProjectInUse):
		http.Error(w, err.Error()+"; a project that tasks belong to cannot be deleted", http.StatusConflict)
	case s.writeProjectError(w, err):
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
