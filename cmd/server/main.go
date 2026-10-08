// Command server runs the orchestrator's server: it keeps the record of
// daemons, tasks, events and commands in a SQLite database and serves the
// daemon-facing and owner-facing HTTP APIs under /v1/ and the owner's GUI
// at /. It has no authentication, so it listens on loopback unless told
// otherwise.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sebnow/orchestrator/internal/server"
)

// shutdownTimeout bounds how long in-flight requests may take to finish
// after a signal.
const shutdownTimeout = 30 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listen := flags.String("listen", "127.0.0.1:8080", "address to serve HTTP on")
	dbPath := flags.String("db", "", "SQLite database file, created with its directory when missing (required)")
	defaultModel := flags.String("default-model", "haiku", "model of a task created without one")
	if err := flags.Parse(args); err != nil {
		return exitCode(err)
	}
	if *dbPath == "" || *defaultModel == "" {
		flags.Usage()
		return 2
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

	srv := server.New(store, log, *defaultModel)
	httpServer := &http.Server{
		Handler: srv,
		// No write timeout: command streams stay open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelError),
	}
	httpServer.RegisterOnShutdown(srv.EndStreams)
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Error("listen", "error", err)
		return 1
	}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	log.Info("serving", "address", listener.Addr().String(), "db", *dbPath)

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
