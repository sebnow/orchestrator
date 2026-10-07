// Command daemon runs one task on this machine for manual use: it starts
// Claude Code on the prompt, prints every journaled event to stdout as a
// JSON line, waits for the turn to end, and stops the harness. It does not
// connect to a server.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sebnow/orchestrator/internal/daemon"
	"github.com/sebnow/orchestrator/internal/harness"
	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/protocol"
)

func main() {
	os.Exit(run())
}

func run() int {
	prompt := flag.String("prompt", "", "the task's prompt (required)")
	workdir := flag.String("workdir", ".", "directory the harness works in")
	model := flag.String("model", "haiku", "model for the harness")
	stateDir := flag.String("state-dir", "", "directory for task journals (required)")
	systemPromptFile := flag.String("system-prompt-file", "", "file whose text is added to the harness's system prompt")
	taskName := flag.String("task", "", "task id (default: task-<UTC timestamp>)")
	claudePath := flag.String("claude", "claude", "path of the claude executable")
	permissions := flag.String("permissions", "ask", "how to answer permission requests: ask, allow or deny")
	flag.Parse()

	if *prompt == "" || *stateDir == "" || !slices.Contains([]string{"ask", "allow", "deny"}, *permissions) {
		flag.Usage()
		return 2
	}
	if *taskName == "" {
		*taskName = "task-" + time.Now().UTC().Format("20060102T150405")
	}
	id, err := protocol.ParseTaskID(*taskName)
	if err != nil {
		return fail(err)
	}
	var systemPrompt string
	if *systemPromptFile != "" {
		text, err := os.ReadFile(*systemPromptFile)
		if err != nil {
			return fail(err)
		}
		systemPrompt = string(text)
	}
	dir, err := filepath.Abs(*workdir)
	if err != nil {
		return fail(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	h, err := claude.New(ctx, *claudePath)
	if err != nil {
		return fail(err)
	}
	gateway, err := daemon.StartGateway()
	if err != nil {
		return fail(err)
	}
	defer gateway.Close()

	var printMu sync.Mutex
	out := json.NewEncoder(os.Stdout)
	out.SetEscapeHTML(false)
	d := daemon.New(*stateDir, h, gateway, func(event protocol.Event) {
		printMu.Lock()
		defer printMu.Unlock()
		out.Encode(event)
	})
	// The harness outlives ctx so that an interrupt signal stops it
	// gracefully below rather than killing it.
	task, err := d.StartTask(context.Background(), daemon.TaskSpec{
		ID:           id,
		Prompt:       *prompt,
		Workdir:      dir,
		Model:        *model,
		SystemPrompt: systemPrompt,
		// This command never pauses the task; the limits are required anyway.
		Pause: daemon.PauseLimits{Acknowledge: 2 * time.Minute, Cleanup: 5 * time.Minute},
	})
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(os.Stderr, "journal: %s\n", daemon.JournalPath(*stateDir, id))
	go answerPermissions(ctx, task, *permissions)

	task.WaitFor(ctx, func(s daemon.State) bool { return !s.Running || (s.TurnsEnded > 0 && !s.Busy) })
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer stopCancel()
	exit := task.Stop(stopCtx)
	if exit.ExitCode != 0 {
		fmt.Fprintf(os.Stderr, "harness exited with %d: %s\n%s", exit.ExitCode, exit.Error, exit.Stderr)
		return 1
	}
	return 0
}

func answerPermissions(ctx context.Context, task *daemon.Task, mode string) {
	terminal := bufio.NewReader(os.Stdin)
	for {
		s, err := task.WaitFor(ctx, func(s daemon.State) bool { return len(s.Permissions) > 0 || !s.Running })
		if err != nil || !s.Running {
			return
		}
		for _, req := range s.Permissions {
			decision := harness.Decision{Allow: mode == "allow", Message: "Denied by the operator."}
			if mode == "ask" {
				fmt.Fprintf(os.Stderr, "allow %s %s? [y/N] ", req.Tool, req.Input)
				answer, _ := terminal.ReadString('\n')
				decision.Allow = strings.EqualFold(strings.TrimSpace(answer), "y")
			}
			task.AnswerPermission(req.RequestID, decision)
		}
	}
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "daemon:", err)
	return 1
}
