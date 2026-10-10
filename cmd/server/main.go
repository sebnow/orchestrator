// Command server runs the orchestrator's server: it keeps the record of
// daemons, tasks, events and commands in a SQLite database and serves the
// daemon-facing and owner-facing HTTP APIs under /v1/ and the owner's GUI
// at /. It serves TLS, authenticates daemons by their client certificates
// and the owner by a token (docs/adr/2026-10-08-daemon-authentication.md,
// docs/adr/2026-10-08-owner-authentication.md), and refuses to start
// without them unless told to serve plain HTTP on loopback with
// -insecure-loopback.
//
// Its subcommands create the certificate authority, issue certificates
// and issue the owner token.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/server"
)

// shutdownTimeout bounds how long in-flight requests may take to finish
// after a signal.
const shutdownTimeout = 30 * time.Second

// minDaemonTimeout is the shortest -daemon-timeout the server accepts.
const minDaemonTimeout = time.Minute

// permissionPolicies are the policies -permissions names
// (docs/adr/2026-10-08-permission-policy.md).
var permissionPolicies = map[string]server.Policy{
	"allow-all": server.AllowAll{},
	"ask":       server.AskOwner{},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// subcommands are the commands the first argument can name instead of
// a flag; without one, the server serves.
var subcommands = map[string]func(args []string, stdout, stderr io.Writer) int{
	"init-ca":           initCA,
	"issue-server-cert": issueServerCert,
	"issue-daemon-cert": issueDaemonCert,
	"issue-owner-token": issueOwnerToken,
	"enrol-token":       enrolToken,
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		subcommand, ok := subcommands[args[0]]
		if !ok {
			fmt.Fprintf(stderr, "server: unknown subcommand %q\n", args[0])
			printSubcommands(stderr)
			return 2
		}
		return subcommand(args[1:], stdout, stderr)
	}
	return serve(args, stderr)
}

func printSubcommands(w io.Writer) {
	names := slices.Sorted(maps.Keys(subcommands))
	fmt.Fprintf(w, "Subcommands, each with its own -h: %s\n", strings.Join(names, ", "))
}

// serve runs the server until SIGINT or SIGTERM.
func serve(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, "Usage: server [flags]")
		flags.PrintDefaults()
		printSubcommands(stderr)
	}
	listen := flags.String("listen", "127.0.0.1:8080", "address to serve on")
	dbPath := flags.String("db", "", "SQLite database file, created with its directory when missing (required)")
	defaultModel := flags.String("default-model", "haiku", "model of a task created without one")
	tlsCert := flags.String("tls-cert", "", "the server's certificate, from issue-server-cert (required unless -insecure-loopback)")
	tlsKey := flags.String("tls-key", "", "the server certificate's key (required unless -insecure-loopback)")
	clientCA := flags.String("client-ca", "", "the CA certificate that daemons' certificates are verified against, from init-ca (required unless -insecure-loopback)")
	caKey := flags.String("ca-key", "", "the key of the -client-ca certificate, from init-ca; turns on enrolment, where daemons get their certificates from the server")
	insecure := flags.Bool("insecure-loopback", false, "serve plain HTTP without authentication, for development; -listen must be a loopback IP address")
	fillerThreshold := flags.Float64("filler-threshold", server.DefaultSchedulePolicy.FillerThreshold, "five-hour window utilization, from 0 to 1, below which filler tasks run")
	lowThreshold := flags.Float64("low-threshold", server.DefaultSchedulePolicy.LowThreshold, "five-hour window utilization, from 0 to 1, below which low-priority tasks run")
	daemonTimeout := flags.Duration("daemon-timeout", server.DefaultDaemonTimeout, "how long a daemon may go unseen, with no command stream open, before it is lost and its tasks move to other daemons; at least 1m")
	permissions := flags.String("permissions", "allow-all", "who answers the agents' permission requests: allow-all, the server, allowing every one at once; or ask, the owner")
	if err := flags.Parse(args); err != nil {
		return exitCode(err)
	}
	if *dbPath == "" || *defaultModel == "" || flags.NArg() > 0 {
		flags.Usage()
		return 2
	}
	// A daemon retries every 30 s at most and notices a dead command
	// stream within 45 s, so a shorter timeout could declare a daemon lost
	// while it reconnects.
	if *daemonTimeout < minDaemonTimeout {
		fmt.Fprintf(stderr, "server: -daemon-timeout must be at least %s\n", minDaemonTimeout)
		return 2
	}
	policy, ok := permissionPolicies[*permissions]
	if !ok {
		fmt.Fprintf(stderr, "server: -permissions must be allow-all or ask, not %q\n", *permissions)
		return 2
	}
	for name, threshold := range map[string]float64{"-filler-threshold": *fillerThreshold, "-low-threshold": *lowThreshold} {
		if threshold < 0 || threshold > 1 {
			fmt.Fprintf(stderr, "server: %s must be from 0 to 1\n", name)
			return 2
		}
	}
	var tlsConfig *tls.Config
	var ca *pki.CA
	if *insecure {
		if *tlsCert != "" || *tlsKey != "" || *clientCA != "" || *caKey != "" {
			fmt.Fprintln(stderr, "server: -insecure-loopback serves plain HTTP; drop -tls-cert, -tls-key, -client-ca and -ca-key")
			return 2
		}
		if err := requireLoopback(*listen); err != nil {
			fmt.Fprintln(stderr, "server: -insecure-loopback:", err)
			return 2
		}
	} else {
		if *tlsCert == "" || *tlsKey == "" || *clientCA == "" {
			fmt.Fprintln(stderr, "server: -tls-cert, -tls-key and -client-ca are required unless -insecure-loopback is given")
			return 2
		}
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			fmt.Fprintln(stderr, "server: load the server certificate:", err)
			return 1
		}
		clientCAs, err := pki.LoadPool(*clientCA)
		if err != nil {
			fmt.Fprintln(stderr, "server: -client-ca:", err)
			return 1
		}
		tlsConfig = pki.ServerConfig(cert, clientCAs)
		if *caKey != "" {
			if ca, err = pki.LoadCAFiles(*clientCA, *caKey); err != nil {
				fmt.Fprintln(stderr, "server: -ca-key:", err)
				return 1
			}
		}
	}
	log := slog.New(slog.NewTextHandler(stderr, nil))

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		log.Error("create database directory", "error", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := server.OpenStore(ctx, *dbPath)
	if err != nil {
		log.Error("open database", "error", err)
		return 1
	}
	defer store.Close()
	if !*insecure {
		issued, err := store.HasOwnerToken(ctx)
		if err != nil {
			log.Error("read owner token", "error", err)
			return 1
		}
		if !issued {
			log.Error("no owner token; issue one with: server issue-owner-token -db " + *dbPath)
			return 1
		}
	}

	srv := server.New(store, log, server.Options{
		DefaultModel: *defaultModel,
		Insecure:     *insecure,
		Schedule: server.SchedulePolicy{FillerThreshold: *fillerThreshold, LowThreshold: *lowThreshold,
			DaemonTimeout: *daemonTimeout},
		Permissions: policy,
		CA:          ca,
	})
	httpServer := &http.Server{
		Handler:   srv,
		TLSConfig: tlsConfig,
		// No write timeout: command streams stay open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
	httpServer.RegisterOnShutdown(srv.EndStreams)
	scheduleCtx, stopScheduling := context.WithCancel(context.Background())
	scheduled := make(chan struct{})
	go func() {
		defer close(scheduled)
		srv.Schedule(scheduleCtx)
	}()
	// The scheduler writes to the store, so it stops before the store
	// closes.
	defer func() {
		stopScheduling()
		<-scheduled
	}()
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Error("listen", "error", err)
		return 1
	}
	served := make(chan error, 1)
	go func() {
		if *insecure {
			served <- httpServer.Serve(listener)
			return
		}
		served <- httpServer.ServeTLS(listener, "", "")
	}()
	log.Info("serving", "address", listener.Addr().String(), "tls", !*insecure, "db", *dbPath)

	select {
	case err := <-served:
		log.Error("serve", "error", err)
		return 1
	case <-ctx.Done():
	}
	// A second signal now ends the process at once.
	stop()
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("shut down", "error", err)
		return 1
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "error", err)
		return 1
	}
	return 0
}

// exitCode is the status of a run whose flags did not parse: 0 when help
// was asked for, as with flag.ExitOnError, and 2 otherwise.
func exitCode(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	return 2
}

// requireLoopback refuses a listen address whose host is not a loopback
// IP address. A host name is refused too, since what it resolves to can
// change.
func requireLoopback(listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.IsLoopback() {
		return fmt.Errorf("-listen %s is not on a loopback IP address such as 127.0.0.1 or [::1]", listen)
	}
	return nil
}
