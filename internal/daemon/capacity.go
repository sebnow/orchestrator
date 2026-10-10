package daemon

import (
	"bufio"
	"context"
	"io"
	"maps"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// The daemon reports how many tasks it can run at once as its slots fact
// (docs/adr/2026-10-10-agent-models-and-capacity.md), from the memory
// the machine has available and its load average. The rule is a
// starting point, not a measurement of what a harness needs:
//
//	slots = min(running + floor(available / 1.5 GiB), cpus)
//	        - ceil(load1 - cpus), when the 1-minute load average exceeds cpus
//
// and never below 0. running counts the harness processes the daemon
// runs: the memory they hold is no longer available, so without them a
// daemon would count its own tasks against itself.

const (
	// slotMemory is the memory one more task is taken to need.
	slotMemory = 3 << 29 // 1.5 GiB
	// capacityInterval is how often the daemon recomputes its slots
	// besides each time a harness starts or exits.
	capacityInterval = 30 * time.Second
)

// machineLoad is what the slots rule reads of the machine: the memory
// available to new processes, in bytes, and the 1-minute load average.
type machineLoad struct {
	Available uint64
	Load1     float64
}

// slotsFor applies the slots rule to a machine with cpus logical CPUs
// and load, running harness processes.
func slotsFor(load machineLoad, cpus, running int) int {
	slots := min(running+int(load.Available/slotMemory), cpus)
	if over := load.Load1 - float64(cpus); over > 0 {
		slots -= int(math.Ceil(over))
	}
	return max(slots, 0)
}

// measureLoad reads the machine's available memory and load average:
// MemAvailable of /proc/meminfo and the first field of /proc/loadavg on
// Linux; on macOS the free, inactive and speculative pages of vm_stat,
// which the kernel can hand to a new process, and sysctl's vm.loadavg.
// It reports false where it cannot read them.
func measureLoad(ctx context.Context) (machineLoad, bool) {
	switch runtime.GOOS {
	case "linux":
		meminfo, err := os.ReadFile("/proc/meminfo")
		if err != nil {
			return machineLoad{}, false
		}
		loadavg, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			return machineLoad{}, false
		}
		available, okMemory := memAvailable(strings.NewReader(string(meminfo)))
		load1, okLoad := firstLoad(string(loadavg))
		return machineLoad{Available: available, Load1: load1}, okMemory && okLoad
	case "darwin":
		vmstat, err := exec.CommandContext(ctx, command("vm_stat", "/usr/bin/vm_stat")).Output()
		if err != nil {
			return machineLoad{}, false
		}
		loadavg, err := exec.CommandContext(ctx, command("sysctl", "/usr/sbin/sysctl"), "-n", "vm.loadavg").Output()
		if err != nil {
			return machineLoad{}, false
		}
		available, okMemory := vmStatAvailable(string(vmstat))
		load1, okLoad := firstLoad(strings.Trim(strings.TrimSpace(string(loadavg)), "{}"))
		return machineLoad{Available: available, Load1: load1}, okMemory && okLoad
	}
	return machineLoad{}, false
}

// command is name's path on PATH, or fallback when it is not there.
func command(name, fallback string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	return fallback
}

// memAvailable reads MemAvailable, in kibibytes, from /proc/meminfo and
// returns it in bytes.
func memAvailable(meminfo io.Reader) (uint64, bool) {
	scanner := bufio.NewScanner(meminfo)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && fields[0] == "MemAvailable:" && fields[2] == "kB" {
			kib, err := strconv.ParseUint(fields[1], 10, 64)
			return kib * 1024, err == nil
		}
	}
	return 0, false
}

// firstLoad reads the 1-minute load average, the first field of text.
func firstLoad(text string) (float64, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return 0, false
	}
	load, err := strconv.ParseFloat(fields[0], 64)
	return load, err == nil
}

// vmStatAvailable reads vm_stat's output and returns the bytes of its
// free, inactive and speculative pages.
func vmStatAvailable(vmstat string) (uint64, bool) {
	header, rest, ok := strings.Cut(vmstat, "\n")
	if !ok {
		return 0, false
	}
	// The header reads "Mach Virtual Memory Statistics: (page size of
	// 16384 bytes)".
	_, size, ok := strings.Cut(header, "page size of ")
	if !ok {
		return 0, false
	}
	pageSize, err := strconv.ParseUint(strings.Fields(size)[0], 10, 64)
	if err != nil {
		return 0, false
	}
	wanted := map[string]bool{"Pages free": true, "Pages inactive": true, "Pages speculative": true}
	var pages uint64
	for line := range strings.SplitSeq(rest, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !wanted[strings.TrimSpace(name)] {
			continue
		}
		count, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
		if err != nil {
			return 0, false
		}
		delete(wanted, strings.TrimSpace(name))
		pages += count
	}
	return pages * pageSize, len(wanted) == 0
}

// refreshSlots recomputes the daemon's slots fact, leaving it out when
// the machine cannot be read, and sends the server its facts when they
// changed. A report that fails is logged; the next connection reports
// the facts again.
func (s *service) refreshSlots(ctx context.Context) {
	facts := s.currentFacts()
	delete(facts, protocol.FactSlots)
	if slots, ok := s.cfg.slots(ctx, s.harnesses.Load()); ok {
		facts[protocol.FactSlots] = strconv.Itoa(slots)
	}
	s.factsMu.Lock()
	changed := !maps.Equal(facts, s.facts)
	s.facts = facts
	s.factsMu.Unlock()
	if !changed {
		return
	}
	s.log.Info("capacity changed", "slots", facts[protocol.FactSlots], "harnesses", s.harnesses.Load())
	if err := s.reportFacts(ctx); err != nil {
		s.log.Warn("report facts", "error", err)
	}
}

// watchCapacity refreshes the slots fact every capacityInterval and each
// time capacityChanged is signalled, until ctx ends.
func (s *service) watchCapacity(ctx context.Context) {
	ticker := time.NewTicker(capacityInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.capacityChanged:
		}
		s.refreshSlots(ctx)
	}
}

// harnessesChanged records that a harness process started, delta 1, or
// exited, delta -1, and asks for the slots to be recomputed.
func (s *service) harnessesChanged(delta int64) {
	s.harnesses.Add(delta)
	select {
	case s.capacityChanged <- struct{}{}:
	default:
	}
}

// machineSlots is the slots rule applied to this machine with running
// harness processes, or false where the machine cannot be read.
func machineSlots(ctx context.Context, running int64) (int, bool) {
	load, ok := measureLoad(ctx)
	if !ok {
		return 0, false
	}
	return slotsFor(load, runtime.NumCPU(), int(running)), true
}
