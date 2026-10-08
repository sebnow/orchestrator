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
	"syscall"

	"github.com/sebnow/orchestrator/internal/daemon"
	"github.com/sebnow/orchestrator/internal/harness/claude"
	"github.com/sebnow/orchestrator/internal/pki"
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
	claudePath := flag.String("claude", "claude", "path of the claude executable")
	flag.Parse()
	if *serverURL == "" || *certPath == "" || *keyPath == "" || *stateDir == "" || flag.NArg() > 0 {
		flag.Usage()
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

	log.Info("connecting", "server", server.String(), "daemon", daemonID)
	err = daemon.Serve(ctx, daemon.Config{
		Server:   server,
		ID:       daemonID,
		StateDir: *stateDir,
		Harness:  h,
		Gateway:  gateway,
		Log:      log,
		Client:   &http.Client{Transport: transport},
	})
	if err != nil {
		log.Error("serve", "error", err)
		return 1
	}
	return 0
}
