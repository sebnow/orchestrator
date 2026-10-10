package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// errRunning reports a request that needs the task's turn to have ended.
var errRunning = errors.New("task's turn has not ended")

// successors returns the tasks that continue task, oldest first. Stored
// times do not sort as text; julianday compares the instants, and the
// row id breaks ties.
func successors(ctx context.Context, tx *sql.Tx, task protocol.TaskID) ([]protocol.TaskID, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM tasks WHERE continues = ? ORDER BY julianday(created_at), rowid`, string(task))
	if err != nil {
		return nil, fmt.Errorf("read the successors of task %q: %w", task, err)
	}
	defer rows.Close()
	var ids []protocol.TaskID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read the successors of task %q: %w", task, err)
		}
		ids = append(ids, protocol.TaskID(id))
	}
	return ids, rows.Err()
}

// predecessor is what a task that continues another takes from it: the
// task's own detail, and the text its hand-back would carry, its final
// reply and where its work is.
type predecessor struct {
	detail taskDetail
	reply  string
}

// readPredecessor reads task for a task to continue it, or returns
// errRunning while a turn of it runs or waits to start.
func (s *Store) readPredecessor(ctx context.Context, task protocol.TaskID) (predecessor, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return predecessor{}, fmt.Errorf("read task %q: %w", task, err)
	}
	defer tx.Rollback()
	detail, err := readTaskDetail(ctx, tx, task)
	if err != nil {
		return predecessor{}, err
	}
	if !detail.State.Idle() || detail.State == TaskQueued {
		return predecessor{}, fmt.Errorf("%w: %q is %s", errRunning, task, detail.State)
	}
	reply, err := handBackText(ctx, tx, task)
	if err != nil {
		return predecessor{}, err
	}
	return predecessor{detail: detail, reply: reply}, nil
}

// continueTask starts a new task that continues task: in the same
// project, as the same agent, with the same model, tools, pause limits,
// purpose, required labels, priority and filler flag, and, outside a
// project, in the same workspace. Its prompt is task's final reply, as
// its hand-back would carry it, and it names task as its predecessor
// (owner decision, 2026-10-10). The new task is a fresh session: none of
// task's conversation comes with it but that reply. It returns the new
// task's queued start, or errRunning while a turn of task runs.
func (s *Server) continueTask(ctx context.Context, task protocol.TaskID) (queuedTurn, error) {
	old, err := s.store.readPredecessor(ctx, task)
	if err != nil {
		return queuedTurn{}, err
	}
	d := old.detail
	start := protocol.StartTask{Prompt: old.reply, Model: d.Model, PauseLimits: d.Start.PauseLimits, Tools: d.Start.Tools}
	if d.Project == "" {
		start.Workspace = d.Start.Workspace
	}
	requires := maps.Clone(d.Requires)
	if requires == nil {
		requires = Labels{}
	}
	filler := d.Filler
	return s.startTask(ctx, taskRequest{
		Agent: d.Agent, Project: d.Project, Purpose: d.Purpose, Requires: &requires, Priority: d.Priority, Filler: &filler,
		Start: start, Continues: &task,
	})
}

// postContinue starts a task that continues the path's task and returns
// its queued start with 201. A task whose turn runs gets 409.
func (s *Server) postContinue(w http.ResponseWriter, r *http.Request) {
	task, err := protocol.ParseTaskID(r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	turn, err := s.continueTask(r.Context(), task)
	if !s.writeContinueError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, turn)
}

// postContinueForm starts a task that continues the path's task and
// sends the browser to the new task's page.
func (s *Server) postContinueForm(w http.ResponseWriter, r *http.Request) {
	task, err := protocol.ParseTaskID(r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	turn, err := s.continueTask(r.Context(), task)
	if !s.writeContinueError(w, err) {
		return
	}
	redirect(w, r, "/tasks/"+string(turn.TaskID))
}

// writeContinueError writes the response for err, if it is not nil, and
// reports whether the request may go on.
func (s *Server) writeContinueError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, errUnknownTask):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, errRunning):
		http.Error(w, err.Error()+"; continue it once the turn has ended", http.StatusConflict)
	case errors.Is(err, errUnknownAgent), errors.Is(err, errUnknownProject), errors.Is(err, errNoDaemon), errors.Is(err, errInvalidTask):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	default:
		s.internalError(w, err)
	}
	return false
}
