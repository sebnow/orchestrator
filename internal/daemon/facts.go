package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
)

// factsTimeout bounds reporting the facts, so that a server that does
// not answer does not hold up the command stream.
const factsTimeout = 10 * time.Second

// detectFacts returns what the daemon can tell about its machine and h,
// the harness it runs (docs/adr/2026-10-09-agents-and-placement.md). A
// fact it cannot detect is left out.
func detectFacts(h protocol.Harness) protocol.Facts {
	facts := protocol.Facts{
		protocol.FactOS:   runtime.GOOS,
		protocol.FactArch: runtime.GOARCH,
		protocol.FactCPUs: strconv.Itoa(runtime.NumCPU()),
	}
	if h.Name != "" {
		facts[protocol.FactHarness] = h.Name
	}
	if h.Version != "" {
		facts[protocol.FactHarnessVersion] = h.Version
	}
	if memory, ok := memoryBytes(); ok {
		facts[protocol.FactMemory] = strconv.FormatUint(memory, 10)
	}
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		facts[protocol.FactGPU] = "apple"
	default:
		if _, err := exec.LookPath("nvidia-smi"); err == nil {
			facts[protocol.FactGPU] = "nvidia"
		}
	}
	return facts
}

// memoryBytes returns the machine's memory: MemTotal of /proc/meminfo on
// Linux, and sysctl's hw.memsize on macOS.
func memoryBytes() (uint64, bool) {
	switch runtime.GOOS {
	case "linux":
		f, err := os.Open("/proc/meminfo")
		if err != nil {
			return 0, false
		}
		defer f.Close()
		return memTotal(f)
	case "darwin":
		sysctl := "/usr/sbin/sysctl"
		if path, err := exec.LookPath("sysctl"); err == nil {
			sysctl = path
		}
		out, err := exec.Command(sysctl, "-n", "hw.memsize").Output()
		if err != nil {
			return 0, false
		}
		memory, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
		return memory, err == nil
	}
	return 0, false
}

// memTotal reads MemTotal, in kibibytes, from /proc/meminfo and returns
// it in bytes.
func memTotal(meminfo io.Reader) (uint64, bool) {
	scanner := bufio.NewScanner(meminfo)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			kib, err := strconv.ParseUint(fields[1], 10, 64)
			return kib * 1024, err == nil
		}
	}
	return 0, false
}

// reportFacts sends the server the daemon's facts. The server keeps the
// latest report, so sending it again changes nothing. Reports may race,
// each sending the facts of its moment, so the last to arrive may be the
// older; the next change or connection sends them again.
func (s *service) reportFacts(ctx context.Context) error {
	body, err := json.Marshal(s.currentFacts())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, factsTimeout)
	defer cancel()
	target := s.cfg.Server.JoinPath("v1", "daemons", string(s.cfg.ID), "facts")
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}
