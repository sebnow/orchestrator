package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
)

func workspaceGone(stateDir string, task protocol.TaskID) func() bool {
	return func() bool {
		_, err := os.Stat(workspacePath(stateDir, task))
		return errors.Is(err, fs.ErrNotExist)
	}
}

// startedTask starts a task through the server and returns it with its
// process, once the process has its prompt and its workspace exists.
func startedTask(t *testing.T, srv *serverFixture, d *daemonFixture) (protocol.TaskID, *fakeProcess) {
	t.Helper()
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)
	if _, err := os.Stat(workspacePath(d.stateDir, task)); err != nil {
		t.Fatalf("workspace of a started task: %v", err)
	}
	return task, proc
}

func TestGivenRunningTaskWhenTheOwnerStopsItThenItsWorkspaceIsDeleted(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task, proc := startedTask(t, srv, d)
	proc.nextInput(t)

	srv.command(t, task, protocol.CommandStop, nil)

	eventually(t, "the workspace to be deleted", workspaceGone(d.stateDir, task))
}

func TestGivenTaskBetweenProcessesWhenTheOwnerStopsItThenItsWorkspaceIsDeleted(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task, proc := startedTask(t, srv, d)
	finishTurn(t, proc, "session-1")
	expectExit(t, proc)
	srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))
	if workspaceGone(d.stateDir, task)() {
		t.Fatal("the workspace of a finished task was deleted")
	}

	srv.command(t, task, protocol.CommandStop, nil)

	eventually(t, "the workspace to be deleted", workspaceGone(d.stateDir, task))
}

func TestGivenRunningTaskWhenItsHarnessFailsThenItsWorkspaceIsDeleted(t *testing.T) {
	srv := startServer(t)
	d := runDaemon(t, srv.url, t.TempDir())
	task, proc := startedTask(t, srv, d)
	proc.nextInput(t)

	proc.end(protocol.HarnessExited{ExitCode: 1})

	eventually(t, "the workspace to be deleted", workspaceGone(d.stateDir, task))
}

func TestGivenRunningTurnWhenTheDaemonShutsDownThenTheWorkspaceOfTheResumableTaskIsKept(t *testing.T) {
	srv := startServer(t)
	stateDir := t.TempDir()
	d := runDaemon(t, srv.url, stateDir)
	task, proc := startedTask(t, srv, d)
	proc.nextInput(t)
	proc.emit(harness.Output{Line: []byte(`{"type":"system","subtype":"init"}`), SessionID: "session-1"})
	srv.waitForEvent(t, task, "the init line", hasSeq(2))

	stopDuringTurn(t, d, proc, protocol.HarnessExited{ExitCode: 1})

	if workspaceGone(stateDir, task)() {
		t.Error("the workspace of a task a shutdown paused was deleted")
	}
}

func TestGivenWorkspacesOfTasksThatCannotRunAgainWhenTheDaemonRecoversThenOnlyTheResumableTasksWorkspaceIsKept(t *testing.T) {
	stateDir := t.TempDir()
	st := mustLoadState(t, stateDir)
	settings := &taskSettings{Prompt: "Do the work.", Model: "haiku", Acknowledge: time.Minute, Cleanup: time.Minute}
	for task, rec := range map[protocol.TaskID]taskRecord{
		"resumable": {Seq: 3, Acked: 3, Session: "session-1", Settings: settings},
		"ended":     {Seq: 3, Acked: 2, Session: "session-2", Settings: settings, Ended: true},
	} {
		if err := st.updateTask(task, func(r *taskRecord) { *r = rec }); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range []protocol.TaskID{"resumable", "ended", "forgotten"} {
		if err := os.MkdirAll(filepath.Join(workspacePath(stateDir, task), "src"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	notATask := filepath.Join(stateDir, "workspaces", "not a task")
	if err := os.MkdirAll(notATask, 0o700); err != nil {
		t.Fatal(err)
	}

	recoverWith(t, stateDir, newFakeProcesses(), time.Second)

	for task, kept := range map[protocol.TaskID]bool{"resumable": true, "ended": false, "forgotten": false} {
		if gone := workspaceGone(stateDir, task)(); gone == kept {
			t.Errorf("workspace of %s deleted = %v, want %v", task, gone, !kept)
		}
	}
	if _, err := os.Stat(notATask); err != nil {
		t.Errorf("directory not named after a task: %v, want it left alone", err)
	}
}
