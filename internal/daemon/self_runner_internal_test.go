package daemon

import (
	"context"
	"path/filepath"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// These run the workspace commands as the daemon's own user, as a daemon
// without a harness user does. The daemon's mirrors of a workspace at
// dir are under a directory ".daemon" beside it, so that the helpers
// given the same workspace share them.

func workspacePath(stateDir string, task protocol.TaskID) string {
	return filepath.Join(stateDir, "workspaces", string(task))
}

// selfRunner is the runner of a daemon without a harness user whose
// workspace is dir.
func selfRunner(dir string) runner {
	return runner{mirrors: newMirrors(filepath.Join(filepath.Dir(dir), ".daemon"), "", false)}
}

func prepareWorkspace(ctx context.Context, dir string, task protocol.TaskID, ws *protocol.Workspace, gitName, gitEmail string) error {
	return selfRunner(dir).prepareWorkspace(ctx, dir, task, ws, gitName, gitEmail)
}

func deliver(ctx context.Context, dir string, task protocol.TaskID) *protocol.BranchPushed {
	return selfRunner(dir).deliver(ctx, dir, task)
}

// pushBranch pushes branch from dir, the workspace of deliveryTask,
// through its mirror.
func pushBranch(ctx context.Context, dir, branch, startRef, remoteHead string) error {
	r := selfRunner(dir)
	repo, err := r.mirrors.recorded(deliveryTask)
	if err != nil {
		return err
	}
	return r.pushBranch(ctx, r.mirrors.path(repo), dir, branch, startRef, remoteHead)
}

func deleteWorkspace(stateDir string, task protocol.TaskID) error {
	dir := workspacePath(stateDir, task)
	return selfRunner(dir).deleteWorkspace(dir, task)
}
