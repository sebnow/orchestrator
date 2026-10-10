package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sebnow/orchestrator/internal/protocol"
)

const gib = 1 << 30

func TestGivenMachineLoadWhenApplyingTheSlotsRuleThenMemoryCPUsLoadAndRunningHarnessesCount(t *testing.T) {
	for name, tc := range map[string]struct {
		load          machineLoad
		cpus, running int
		want          int
	}{
		"memory bounds":                {machineLoad{Available: 7 * gib, Load1: 1}, 8, 0, 4},
		"cpus bound":                   {machineLoad{Available: 64 * gib, Load1: 1}, 8, 0, 8},
		"running harnesses count back": {machineLoad{Available: 3 * gib, Load1: 2}, 8, 3, 5},
		"load over cpus reduces":       {machineLoad{Available: 64 * gib, Load1: 10.2}, 8, 0, 5},
		"load at cpus does not":        {machineLoad{Available: 64 * gib, Load1: 8}, 8, 0, 8},
		"never below zero":             {machineLoad{Available: gib, Load1: 30}, 8, 0, 0},
	} {
		if got := slotsFor(tc.load, tc.cpus, tc.running); got != tc.want {
			t.Errorf("%s: slots = %d, want %d", name, got, tc.want)
		}
	}
}

func TestGivenProcMeminfoAndLoadavgWhenReadThenAvailableMemoryAndTheOneMinuteLoadAreFound(t *testing.T) {
	available, ok := memAvailable(strings.NewReader("MemTotal: 16314208 kB\nMemFree: 1048576 kB\nMemAvailable: 8157104 kB\n"))
	if !ok || available != 8157104*1024 {
		t.Errorf("MemAvailable = %d, %v", available, ok)
	}
	if _, ok := memAvailable(strings.NewReader("MemTotal: 1 kB\n")); ok {
		t.Error("found MemAvailable in meminfo without one")
	}
	if load, ok := firstLoad("2.50 1.75 1.10 3/812 40213\n"); !ok || load != 2.5 {
		t.Errorf("load = %v, %v", load, ok)
	}
	if load, ok := firstLoad(strings.Trim("{ 3.17 2.90 2.71 }", "{}")); !ok || load != 3.17 {
		t.Errorf("sysctl load = %v, %v", load, ok)
	}
}

func TestGivenVMStatOutputWhenReadThenFreeInactiveAndSpeculativePagesAreAvailable(t *testing.T) {
	const vmstat = "Mach Virtual Memory Statistics: (page size of 16384 bytes)\n" +
		"Pages free:                               10000.\n" +
		"Pages active:                            500000.\n" +
		"Pages inactive:                           20000.\n" +
		"Pages speculative:                         3000.\n" +
		"Pages wired down:                         90000.\n"

	available, ok := vmStatAvailable(vmstat)

	if !ok || available != 33000*16384 {
		t.Errorf("available = %d, %v; want %d", available, ok, 33000*16384)
	}
	if _, ok := vmStatAvailable("Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free: 1.\n"); ok {
		t.Error("vm_stat output without inactive and speculative pages was read")
	}
}

func TestGivenSlotsThatChangeWhenAHarnessStartsAndExitsThenTheDaemonReportsItsFactsAgain(t *testing.T) {
	srv := startServer(t)
	var running atomic.Int64
	d := runDaemonAs(t, srv.url, t.TempDir(), func(cfg *Config) {
		cfg.measureSlots = func(_ context.Context, harnesses int64) (int, bool) {
			running.Store(harnesses)
			return 4 - int(harnesses), true
		}
	})
	slotsFact := func() string {
		var view struct {
			Facts map[string]string `json:"facts"`
		}
		status, body := srv.try(t, http.MethodGet, "/v1/daemons/"+string(testDaemon), nil)
		if status != http.StatusOK || json.Unmarshal(body, &view) != nil {
			return ""
		}
		return view.Facts[protocol.FactSlots]
	}
	eventually(t, "the first report", func() bool { return slotsFact() == "4" })

	task := srv.createTask(t, testDaemon, protocol.StartTask{Prompt: "Work.", PauseLimits: testPauseLimits})
	proc := d.nextProcess(t)
	eventually(t, "the report once the harness started", func() bool { return slotsFact() == "3" })
	finishTurn(t, proc, "session-1")
	expectExit(t, proc)
	srv.waitForEvent(t, task, "harness_exited", isKind(protocol.KindHarnessExited))

	eventually(t, "the report once the harness exited", func() bool { return slotsFact() == "4" })
	if got := running.Load(); got != 0 {
		t.Errorf("harnesses counted = %d, want 0", got)
	}
}
