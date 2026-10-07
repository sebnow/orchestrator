// Command daemon runs the orchestrator's tasks on this machine: it
// connects to the server, runs Claude Code for each task the server
// starts, applies the server's commands, and sends the server every
// event. It has no authentication, so the server should be on loopback.
//
// On SIGINT or SIGTERM it stops its running tasks, sends their last
// events, and exits; a second signal ends it at once.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/sebnow/orchestrator/internal/daemon"
	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/protocol"
)

func main() {
	os.Exit(run())
}

func run() int {
	serverURL := flag.String("server", "", "base URL of the server, such as http://127.0.0.1:8080 (required)")
	id := flag.String("id", "", "this daemon's id: letters, digits, '.', '_' and '-' (required)")
	stateDir := flag.String("state-dir", "", "directory for the daemon's state, task journals and workspaces (required)")
	claudePath := flag.String("claude", "claude", "path of the claude executable")
	flag.Parse()
	if *serverURL == "" || *id == "" || *stateDir == "" {
		flag.Usage()
		return 2
	}
	server, err := url.Parse(*serverURL)
	if err != nil || (server.Scheme != "http" && server.Scheme != "https") || server.Host == "" {
		fmt.Fprintf(os.Stderr, "daemon: -server %q is not an http or https URL\n", *serverURL)
		return 2
	}
	daemonID, err := protocol.ParseDaemonID(*id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon: -id:", err)
		return 2
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// A second signal now ends the process at once.
		stop()
	}()
	h, err := claude.New(ctx, *claudePath)
	if err != nil {
		log.Error("find claude", "error", err)
		return 1
	}
	gateway, err := daemon.StartGateway()
	if err != nil {
		log.Error("start gateway", "error", err)
		return 1
	}
	defer gateway.Close()

	err = daemon.Serve(ctx, daemon.Config{
		Server:   server,
		ID:       daemonID,
		StateDir: *stateDir,
		Harness:  h,
		Gateway:  gateway,
		Log:      log,
	})
	if err != nil {
		log.Error("serve", "error", err)
		return 1
	}
	return 0
}
