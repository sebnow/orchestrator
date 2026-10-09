package claude_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/protocol"
	"github.com/sebnow/orchestrator/internal/runas"
)

// fakeSudo stands in for sudo running the harness: it says when its
// SIGTERM trap is set, then reads its input to the end. A SIGTERM makes
// it exit 3 once that read ends; the end of input alone makes it exit 4.
const fakeSudo = `#!/bin/sh
trap 'echo sigterm >&2; exit 3' TERM
echo ready
cat >/dev/null
echo "input closed" >&2
exit 4
`

func TestGivenHarnessUserWhenKillingThenSudoIsSentSIGTERMAndTheInputIsClosed(t *testing.T) {
	sudo := filepath.Join(t.TempDir(), "sudo")
	if err := os.WriteFile(sudo, []byte(fakeSudo), 0o700); err != nil {
		t.Fatal(err)
	}
	h := fakeHarness(t)
	spec := testSpec
	spec.Workdir = t.TempDir()
	spec.RunAs = runas.User{Name: "orch-agent", Sudo: sudo}
	proc, err := h.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if out := read(t, proc); string(out.Line) != "ready" {
		t.Fatalf("first line = %q, want ready", out.Line)
	}

	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan protocol.HarnessExited, 1)
	go func() {
		for {
			if _, err := proc.Read(); err != nil {
				break
			}
		}
		exited <- proc.Wait()
	}()

	select {
	case exit := <-exited:
		if exit.ExitCode != 3 || !strings.Contains(exit.Stderr, "sigterm") {
			t.Errorf("exited = %+v; want exit 3 from the SIGTERM trap", exit)
		}
	case <-time.After(10 * time.Second):
		proc.CloseInput()
		t.Fatal("the fake sudo did not exit; its input was not closed")
	}
}
