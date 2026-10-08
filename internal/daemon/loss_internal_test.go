package daemon

import (
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/server"
)

func TestGivenRunningTaskTheServerMovedWhileTheDaemonWasAwayWhenTheDaemonReturnsThenItKillsTheHarnessAndForgetsTheTask(t *testing.T) {
	srv := startServerWith(t, server.Options{Schedule: server.SchedulePolicy{
		SlotsPerDaemon: 2, FillerThreshold: 0.5, LowThreshold: 0.85, DaemonTimeout: 200 * time.Millisecond,
	}})
	proxy := startProxy(t, srv.url)
	d := runDaemon(t, proxy.url(), t.TempDir())
	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Do the work.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)
	proc.nextInput(t)
	held := len(srv.waitForEvent(t, task, "harness_started", isKind(protocol.KindHarnessStarted)))
	if _, err := os.Stat(workspacePath(d.stateDir, task)); err != nil {
		t.Fatalf("workspace before the move: %v", err)
	}

	proxy.cutAll()
	srv.waitForState(t, task, "queued")
	proc.emit(harness.Output{Line: []byte(`{"n":1}`)})
	proxy.restore()

	eventually(t, "the daemon to forget the task", func() bool {
		st, err := loadState(d.stateDir)
		return err == nil && !st.known(task)
	})
	if !proc.killed.Load() {
		t.Error("the harness of the moved task was not killed")
	}
	for what, path := range map[string]string{"workspace": workspacePath(d.stateDir, task), "journal": JournalPath(d.stateDir, task)} {
		if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s of the moved task: %v, want it deleted", what, err)
		}
	}
	if got := len(srv.events(t, task)); got != held {
		t.Errorf("server holds %d events of the moved task, want the %d it held before the move", got, held)
	}
}
