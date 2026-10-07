// Command server runs the orchestrator's server: it keeps the record of
// daemons, tasks, events and commands in a SQLite database and serves the
// daemon-facing and owner-facing HTTP APIs. It has no authentication, so
// it listens on loopback unless told otherwise.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sebnow/orchestrator/internal/server"
)

// shutdownTimeout bounds how long in-flight requests may take to finish
// after a signal.
const shutdownTimeout = 30 * time.Second

func main() {
	os.Exit(run())
}

func run() int {
	listen := flag.String("listen", "127.0.0.1:8080", "address to serve HTTP on")
	dbPath := flag.String("db", "", "SQLite database file, created when missing (required)")
	flag.Parse()
	if *dbPath == "" {
		flag.Usage()
		return 2
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	store, err := server.OpenStore(ctx, *dbPath)
	if err != nil {
		log.Error("open database", "error", err)
		return 1
	}
	defer store.Close()

	srv := server.New(store, log)
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
