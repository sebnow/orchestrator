package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// taskNode is a task in a tree of tasks and the tasks it spawned, each
// with what says why it exists and what it has done
// (docs/adr/2026-10-10-projects-and-lineage.md). Children are oldest
// first, and never nil.
type taskNode struct {
	ID       protocol.TaskID        `json:"id"`
	State    TaskState              `json:"state"`
	Agent    string                 `json:"agent,omitempty"`
	Purpose  string                 `json:"purpose,omitempty"`
	CostUSD  float64                `json:"cost_usd"`
	Branch   *protocol.BranchPushed `json:"branch,omitempty"`
	Children []taskNode             `json:"children"`
	// prompt names the task in the GUI when it has no purpose.
	prompt string
}

// tree returns the tree rooted at task, or errUnknownTask. Stored times
// do not sort as text; julianday orders siblings by when they were
// created, to the millisecond, and the row id breaks ties.
func (s *Store) tree(ctx context.Context, task protocol.TaskID) (taskNode, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return taskNode{}, fmt.Errorf("read tree of task %q: %w", task, err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		WITH RECURSIVE subtree (id) AS (
			SELECT id FROM tasks WHERE id = ?1
			UNION ALL
			SELECT t.id FROM tasks t JOIN subtree s ON t.parent_id = s.id)
		SELECT t.id, coalesce(t.parent_id, ''), t.state, coalesce(t.agent, ''), t.purpose, t.cost_usd, t.prompt
		FROM tasks t JOIN subtree s ON t.id = s.id
		ORDER BY julianday(t.created_at), t.rowid`, string(task))
	if err != nil {
		return taskNode{}, fmt.Errorf("read tree of task %q: %w", task, err)
	}
	defer rows.Close()
	var nodes []taskNode
	var parents []protocol.TaskID
	for rows.Next() {
		var node taskNode
		var id, parent, state string
		if err := rows.Scan(&id, &parent, &state, &node.Agent, &node.Purpose, &node.CostUSD, &node.prompt); err != nil {
			return taskNode{}, fmt.Errorf("read tree of task %q: %w", task, err)
		}
		node.ID, node.State, node.Children = protocol.TaskID(id), TaskState(state), []taskNode{}
		nodes, parents = append(nodes, node), append(parents, protocol.TaskID(parent))
	}
	if err := rows.Err(); err != nil {
		return taskNode{}, fmt.Errorf("read tree of task %q: %w", task, err)
	}
	rows.Close()
	if len(nodes) == 0 {
		return taskNode{}, fmt.Errorf("%w: %q", errUnknownTask, task)
	}
	for idx := range nodes {
		branches, err := latestBranches(ctx, tx, nodes[idx].ID)
		if err != nil {
			return taskNode{}, err
		}
		nodes[idx].Branch = branches[nodes[idx].ID]
	}
	return assembleTree(task, nodes, parents), nil
}

// assembleTree builds the tree rooted at root from nodes, each a child of
// the task at the same index of parents, keeping their order among
// siblings.
func assembleTree(root protocol.TaskID, nodes []taskNode, parents []protocol.TaskID) taskNode {
	children := make(map[protocol.TaskID][]int)
	var top int
	for idx, node := range nodes {
		if node.ID == root {
			top = idx
			continue
		}
		children[parents[idx]] = append(children[parents[idx]], idx)
	}
	var build func(idx int) taskNode
	build = func(idx int) taskNode {
		node := nodes[idx]
		for _, child := range children[node.ID] {
			node.Children = append(node.Children, build(child))
		}
		return node
	}
	return build(top)
}

// getTree returns the tree of tasks rooted at the task the path names.
func (s *Server) getTree(w http.ResponseWriter, r *http.Request) {
	task, err := protocol.ParseTaskID(r.PathValue("task"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tree, err := s.store.tree(r.Context(), task)
	if errors.Is(err, errUnknownTask) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tree)
}
