package daemon

import (
	"bytes"
	"log/slog"
	"os/exec"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// fakeProcesses is a process table of fake processes: each runs, with its
// start time, until it has been looked up checks times or is killed.
type fakeProcesses struct {
	mu     sync.Mutex
	procs  map[int]*fakeOSProcess
	killed []int
	// lookedUp, when set, runs on every lookup of a running process.
	lookedUp func(pid int)
}

type fakeOSProcess struct {
	started string
	// checks is how many more lookups find the process running; negative
	// keeps it running.
	checks int
}

func newFakeProcesses() *fakeProcesses {
	return &fakeProcesses{procs: map[int]*fakeOSProcess{}}
}

// run adds a running process that exits after checks lookups, or never
// when checks is negative.
func (f *fakeProcesses) run(pid int, started string, checks int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[pid] = &fakeOSProcess{started: started, checks: checks}
}

func (f *fakeProcesses) started(pid int) (string, error) {
	f.mu.Lock()
	p, ok := f.procs[pid]
	if ok && p.checks == 0 {
		delete(f.procs, pid)
		ok = false
	}
	if ok && p.checks > 0 {
		p.checks--
	}
	lookedUp := f.lookedUp
	f.mu.Unlock()
	if !ok {
		return "", nil
	}
	if lookedUp != nil {
		lookedUp(pid)
	}
	return p.started, nil
}

func (f *fakeProcesses) kill(pid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.procs, pid)
	f.killed = append(f.killed, pid)
	return nil
}

func (f *fakeProcesses) killedPIDs() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.killed)
}

func (f *fakeProcesses) running(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.procs[pid]
	return ok
}

// recoverWith recovers the tasks under stateDir with procs as the process
// table, waiting up to wait for harnesses left running.
func recoverWith(t *testing.T, stateDir string, procs *fakeProcesses, wait time.Duration) (*state, *bytes.Buffer) {
	t.Helper()
	st := mustLoadState(t, stateDir)
	var logs bytes.Buffer
	d := New(stateDir, newFakeHarness(), nil, nil)
	d.processes = procs
	if err := d.recoverTasks(st, slog.New(slog.NewTextHandler(&logs, nil)), wait); err != nil {
		t.Fatal(err)
	}
	return st, &logs
}

// harnessLeftRunning records, as a daemon that dies during task's turn
// leaves it, a process of task whose harness has pid and start time
// started.
func harnessLeftRunning(t *testing.T, stateDir string, task protocol.TaskID, pid int, started string) {
	t.Helper()
	writeJournal(t, stateDir, task, 1)
	settings := &taskSettings{Prompt: "Do the work.", Model: "fake-model"}
	startingProcess(t, stateDir, task, taskRecord{Session: "session-1", Settings: settings, Harness: &harnessProcess{PID: pid, Started: started}})
}

func journalEndsInExit(t *testing.T, stateDir string, task protocol.TaskID) bool {
	t.Helper()
	events := readJournalFile(t, JournalPath(stateDir, task))
	return events[len(events)-1].Kind == protocol.KindHarnessExited
}

func TestGivenHarnessLeftRunningWhenRecoveringThenTheDaemonWaitsForItToExitBeforeReportingTheTurnCutShort(t *testing.T) {
	stateDir := t.TempDir()
	harnessLeftRunning(t, stateDir, "task-1", 77, "Thu Oct  8 18:10:35 2026")
	procs := newFakeProcesses()
	procs.run(77, "Thu Oct  8 18:10:35 2026", 3)
	reportedWhileRunning := false
	procs.lookedUp = func(int) { reportedWhileRunning = reportedWhileRunning || journalEndsInExit(t, stateDir, "task-1") }

	st, logs := recoverWith(t, stateDir, procs, 10*time.Second)

	if procs.running(77) || len(procs.killedPIDs()) != 0 {
		t.Errorf("killed %v; want the harness to have exited by itself", procs.killedPIDs())
	}
	if reportedWhileRunning {
		t.Error("the turn was reported cut short while the harness ran")
	}
	assertEndsWithRestartExit(t, readJournalFile(t, JournalPath(stateDir, "task-1")), 3, oldHarness, restartError)
	if rec, _ := st.record("task-1"); rec.Harness != nil || rec.Ended || !rec.CutShort {
		t.Errorf("record = %+v", rec)
	}
	if !bytes.Contains(logs.Bytes(), []byte("harness left by the previous daemon exited")) {
		t.Errorf("logs = %s", logs)
	}
}

func TestGivenHarnessLeftRunningThatDoesNotExitWhenRecoveringThenItIsKilledAfterTheWait(t *testing.T) {
	stateDir := t.TempDir()
	harnessLeftRunning(t, stateDir, "task-1", 77, "Thu Oct  8 18:10:35 2026")
	harnessLeftRunning(t, stateDir, "task-2", 78, "Thu Oct  8 18:10:36 2026")
	procs := newFakeProcesses()
	procs.run(77, "Thu Oct  8 18:10:35 2026", -1)
	procs.run(78, "Thu Oct  8 18:10:36 2026", -1)
	const wait = 300 * time.Millisecond

	began := time.Now()
	st, logs := recoverWith(t, stateDir, procs, wait)

	// One wait covers every harness left running.
	if took := time.Since(began); took < wait || took > 2*wait {
		t.Errorf("recovery took %s, want about %s", took, wait)
	}
	if killed := procs.killedPIDs(); !slices.Equal(slices.Sorted(slices.Values(killed)), []int{77, 78}) {
		t.Errorf("killed %v, want 77 and 78", killed)
	}
	for _, task := range []protocol.TaskID{"task-1", "task-2"} {
		assertEndsWithRestartExit(t, readJournalFile(t, JournalPath(stateDir, task)), 3, oldHarness, restartError)
		if rec, _ := st.record(task); rec.Harness != nil {
			t.Errorf("%s record = %+v", task, rec)
		}
	}
	if !bytes.Contains(logs.Bytes(), []byte("did not exit; killed it")) {
		t.Errorf("logs = %s", logs)
	}
}

func TestGivenHarnessPIDNowUsedByAnotherProcessWhenRecoveringThenThatProcessIsLeftAlone(t *testing.T) {
	stateDir := t.TempDir()
	harnessLeftRunning(t, stateDir, "task-1", 77, "Thu Oct  8 18:10:35 2026")
	procs := newFakeProcesses()
	procs.run(77, "Thu Oct  8 18:12:01 2026", -1)

	began := time.Now()
	st, _ := recoverWith(t, stateDir, procs, 10*time.Second)

	if !procs.running(77) || len(procs.killedPIDs()) != 0 || time.Since(began) > 5*time.Second {
		t.Errorf("killed %v, took %s; want the other process left alone at once", procs.killedPIDs(), time.Since(began))
	}
	if rec, _ := st.record("task-1"); rec.Harness != nil || !rec.CutShort {
		t.Errorf("record = %+v", rec)
	}
}

func TestGivenHarnessWithoutARecordedStartTimeWhenRecoveringThenItIsLeftAloneAndLogged(t *testing.T) {
	stateDir := t.TempDir()
	harnessLeftRunning(t, stateDir, "task-1", 77, "")
	procs := newFakeProcesses()
	procs.run(77, "Thu Oct  8 18:10:35 2026", -1)

	_, logs := recoverWith(t, stateDir, procs, 10*time.Second)

	if !procs.running(77) || len(procs.killedPIDs()) != 0 {
		t.Errorf("killed %v; want the process left alone", procs.killedPIDs())
	}
	if !bytes.Contains(logs.Bytes(), []byte("cannot be identified")) {
		t.Errorf("logs = %s", logs)
	}
}

func TestGivenChildProcessWhenLookedUpWithPsThenItHasAStableStartTimeUntilKilledAndThenNone(t *testing.T) {
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	t.Cleanup(func() { child.Process.Kill() })
	var procs psTable

	first, err := procs.started(pid)
	if err != nil || first == "" {
		t.Fatalf("started = %q, %v; want a start time", first, err)
	}
	if again, err := procs.started(pid); again != first || err != nil {
		t.Errorf("started again = %q, %v; want %q", again, err, first)
	}
	if err := procs.kill(pid); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err == nil || child.ProcessState.String() != "signal: killed" {
		t.Errorf("child ended with %v", err)
	}
	if gone, err := procs.started(pid); gone != "" || err != nil {
		t.Errorf("started after exit = %q, %v; want none", gone, err)
	}
}
