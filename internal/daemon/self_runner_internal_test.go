package daemon

import (
	"context"
	"path/filepath"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// These run the workspace commands as the daemon's own user, as a daemon
// without a harness user does.

func workspacePath(stateDir string, task protocol.TaskID) string {
	return filepath.Join(stateDir, "workspaces", string(task))
}

func prepareWorkspace(ctx context.Context, dir string, task protocol.TaskID, ws *protocol.Workspace, gitName, gitEmail string) error {
	return runner{}.prepareWorkspace(ctx, dir, task, ws, gitName, gitEmail)
}

func deliver(ctx context.Context, dir string, task protocol.TaskID) *protocol.BranchPushed {
	return runner{}.deliver(ctx, dir, task)
}

func pushBranch(ctx context.Context, dir, branch, startRef, remoteHead string) error {
	return runner{}.pushBranch(ctx, dir, branch, startRef, remoteHead)
}

func deleteWorkspace(stateDir string, task protocol.TaskID) error {
	return runner{}.deleteWorkspace(workspacePath(stateDir, task), task)
}
