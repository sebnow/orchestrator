// Command daemon runs the orchestrator's tasks on this machine: it
// connects to the server, runs Claude Code for each task the server
// starts, applies the server's commands, and sends the server every
// event.
//
// It authenticates to the server with its client certificate, whose
// common name is its id, and trusts only servers whose certificates its
// CA issued (docs/adr/2026-10-08-daemon-authentication.md). A plain
// http:// server URL is accepted only for a loopback IP address, for a
// server run with -insecure-loopback.
//
// With -harness-user it runs Claude Code, and every command that touches
// a workspace, as that OS user through sudo, so that the agent cannot
// read the daemon's key, state or journals
// (docs/adr/2026-10-08-harness-user.md).
//
// On SIGINT or SIGTERM it stops its running tasks, sends their last
// events, and exits; a second signal ends it at once.
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/sebnow/orchestrator/internal/daemon"
	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/runas"
	"github.com/sebnow/orchestrator/internal/sshagent"
)

func main() {
	os.Exit(run())
}

func run() int {
	serverURL := flag.String("server", "", "base URL of the server, such as https://orchestrator.example:8443 (required)")
	certPath := flag.String("cert", "", "this daemon's certificate, from the server's issue-daemon-cert; its common name is the daemon's id (required)")
	keyPath := flag.String("key", "", "the certificate's private key (required)")
	caPath := flag.String("ca", "", "the CA certificate to verify the server with (required for https)")
	stateDir := flag.String("state-dir", "", "directory for the daemon's state, task journals and workspaces (required)")
	workspaceDir := flag.String("workspace-dir", "", "directory for the task workspaces, which must exist and be owned by -harness-user when that is set (required with -harness-user; default <state-dir>/workspaces)")
	harnessUser := flag.String("harness-user", "", "OS user to run claude and every workspace command as, through sudo; empty runs them as the daemon's own user")
	claudePath := flag.String("claude", "claude", "path of the claude executable; with -harness-user, the absolute path the sudoers rule names")
	gitIdentity := flag.String("git-identity", "orchestrator <orchestrator@localhost>", `name and email for the commits an agent makes, as "Name <email>"`)
	flag.Parse()
	if *serverURL == "" || *certPath == "" || *keyPath == "" || *stateDir == "" || flag.NArg() > 0 {
		flag.Usage()
		return 2
	}
	if *harnessUser != "" {
		if *workspaceDir == "" {
			fmt.Fprintln(os.Stderr, "daemon: -workspace-dir is required with -harness-user, because the harness user cannot enter the state directory")
			return 2
		}
		if !filepath.IsAbs(*claudePath) {
			fmt.Fprintln(os.Stderr, "daemon: -claude must be an absolute path with -harness-user, the one the sudoers rule names")
			return 2
		}
	}
	gitName, gitEmail, err := daemon.ParseGitIdentity(*gitIdentity)
	if err != nil {
		fmt.Fprintln(os.Stderr, `daemon: -git-identity must be "Name <email>"`)
		return 2
	}
	server, err := url.Parse(*serverURL)
	if err != nil || (server.Scheme != "http" && server.Scheme != "https") || server.Host == "" {
		fmt.Fprintf(os.Stderr, "daemon: -server %q is not an http or https URL\n", *serverURL)
		return 2
	}
	if server.Scheme == "http" {
		if addr, err := netip.ParseAddr(server.Hostname()); err != nil || !addr.IsLoopback() {
			fmt.Fprintf(os.Stderr, "daemon: -server %q: plain http is allowed only for a loopback IP address such as 127.0.0.1\n", *serverURL)
			return 2
		}
	} else if *caPath == "" {
		fmt.Fprintln(os.Stderr, "daemon: -ca is required for an https server")
		return 2
	}
	cert, err := tls.LoadX509KeyPair(*certPath, *keyPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon: load the daemon certificate:", err)
		return 1
	}
	daemonID, err := pki.DaemonID(cert.Leaf)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon: -cert:", err)
		return 1
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if server.Scheme == "https" {
		roots, err := pki.LoadPool(*caPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "daemon: -ca:", err)
			return 1
		}
		transport.TLSClientConfig = pki.ClientConfig(cert, roots)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// A second signal now ends the process at once.
		stop()
	}()
	runAs := runas.User{Name: *harnessUser}
	if agent := os.Getenv("SSH_AUTH_SOCK"); runAs.Other() && agent != "" {
		forwarder, err := sshagent.Forward(runas.TempDir, agent)
		if err != nil {
			log.Error("forward the ssh agent to the harness user", "error", err)
			return 1
		}
		defer forwarder.Close()
		runAs.SSHAuthSock = forwarder.Path()
	}
	h, err := claude.NewAs(ctx, *claudePath, runAs)
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

	log.Info("connecting", "server", server.String(), "daemon", daemonID)
	err = daemon.Serve(ctx, daemon.Config{
		Server:       server,
		ID:           daemonID,
		StateDir:     *stateDir,
		WorkspaceDir: *workspaceDir,
		HarnessUser:  runAs,
		Harness:      h,
		Gateway:      gateway,
		Log:          log,
		Client:       &http.Client{Transport: transport},
		GitName:      gitName,
		GitEmail:     gitEmail,
	})
	if err != nil {
		log.Error("serve", "error", err)
		return 1
	}
	return 0
}
