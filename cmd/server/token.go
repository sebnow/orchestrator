package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sebnow/orchestrator/internal/server"
)

// issueOwnerToken issues a new owner token into the database, which
// ends every session and makes the previous token useless, and prints
// it once.
func issueOwnerToken(args []string, stdout, stderr io.Writer) int {
	flags := subcommandFlags("issue-owner-token", "-db FILE", stderr)
	dbPath := flags.String("db", "", "the server's SQLite database file, created with its directory when missing (required)")
	if status, ok := parseSubcommand(flags, args, dbPath); !ok {
		return status
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		fmt.Fprintln(stderr, "server issue-owner-token:", err)
		return 1
	}
	ctx := context.Background()
	store, err := server.OpenStore(ctx, *dbPath)
	if err != nil {
		fmt.Fprintln(stderr, "server issue-owner-token:", err)
		return 1
	}
	defer store.Close()
	token, err := store.IssueOwnerToken(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "server issue-owner-token:", err)
		return 1
	}
	fmt.Fprintln(stderr, "The owner token follows. It is not shown again; keep it somewhere safe. Earlier tokens and sessions no longer work.")
	fmt.Fprintln(stdout, token)
	return 0
}
