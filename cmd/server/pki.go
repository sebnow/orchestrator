package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sebnow/orchestrator/internal/pki"
	"github.com/sebnow/orchestrator/internal/protocol"
)

// subcommandFlags is the flag set of a subcommand, printing its usage
// line and flags to stderr.
func subcommandFlags(name, synopsis string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet("server "+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintf(stderr, "Usage: server %s %s\n", name, synopsis)
		flags.PrintDefaults()
	}
	return flags
}

// parseSubcommand parses args and reports the exit status when the
// subcommand cannot run: flags that did not parse, a stray argument, or
// a required flag left empty.
func parseSubcommand(flags *flag.FlagSet, args []string, required ...*string) (int, bool) {
	if err := flags.Parse(args); err != nil {
		return exitCode(err), false
	}
	missing := flags.NArg() > 0
	for _, value := range required {
		missing = missing || *value == ""
	}
	if missing {
		flags.Usage()
		return 2, false
	}
	return 0, true
}

// initCA creates the CA under -pki-dir.
func initCA(args []string, stdout, stderr io.Writer) int {
	flags := subcommandFlags("init-ca", "-pki-dir DIR", stderr)
	dir := flags.String("pki-dir", "", "directory for the CA's certificate and key, created when missing (required)")
	if status, ok := parseSubcommand(flags, args, dir); !ok {
		return status
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		fmt.Fprintln(stderr, "server init-ca:", err)
		return 1
	}
	ca, err := pki.NewCA()
	if err == nil {
		err = ca.Write(*dir)
	}
	if err != nil {
		fmt.Fprintln(stderr, "server init-ca:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote %s and %s.\n", filepath.Join(*dir, "ca.crt"), filepath.Join(*dir, "ca.key"))
	return 0
}

// issueServerCert issues the server's certificate from the CA under
// -pki-dir, for every -host, and writes it beside the CA.
func issueServerCert(args []string, stdout, stderr io.Writer) int {
	flags := subcommandFlags("issue-server-cert", "-pki-dir DIR -host NAME_OR_IP [-host NAME_OR_IP]...", stderr)
	dir := flags.String("pki-dir", "", "directory holding the CA (required)")
	var hosts []string
	flags.Func("host", "a DNS name or IP address daemons and browsers reach the server by; repeat for each (at least one)", func(host string) error {
		hosts = append(hosts, host)
		return nil
	})
	if status, ok := parseSubcommand(flags, args, dir); !ok {
		return status
	}
	if len(hosts) == 0 {
		flags.Usage()
		return 2
	}
	ca, err := pki.LoadCA(*dir)
	if err != nil {
		fmt.Fprintln(stderr, "server issue-server-cert:", err)
		return 1
	}
	issued, err := ca.IssueServer(hosts)
	if err == nil {
		err = issued.Write(*dir, "server")
	}
	if err != nil {
		fmt.Fprintln(stderr, "server issue-server-cert:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote %s and %s for %s.\n",
		filepath.Join(*dir, "server.crt"), filepath.Join(*dir, "server.key"), strings.Join(hosts, ", "))
	return 0
}

// issueDaemonCert issues a daemon's certificate from the CA under
// -pki-dir and writes it, its key and the CA certificate to
// daemons/<id>/ there, for the owner to copy to the daemon's machine.
func issueDaemonCert(args []string, stdout, stderr io.Writer) int {
	flags := subcommandFlags("issue-daemon-cert", "-pki-dir DIR -id DAEMON", stderr)
	dir := flags.String("pki-dir", "", "directory holding the CA (required)")
	id := flags.String("id", "", "the daemon's id: letters, digits, '.', '_' and '-' (required)")
	if status, ok := parseSubcommand(flags, args, dir, id); !ok {
		return status
	}
	daemon, err := protocol.ParseDaemonID(*id)
	if err != nil {
		fmt.Fprintln(stderr, "server issue-daemon-cert: -id:", err)
		return 2
	}
	ca, err := pki.LoadCA(*dir)
	if err != nil {
		fmt.Fprintln(stderr, "server issue-daemon-cert:", err)
		return 1
	}
	out := filepath.Join(*dir, "daemons", string(daemon))
	issued, err := ca.IssueDaemon(daemon)
	if err == nil {
		err = os.MkdirAll(out, 0o700)
	}
	if err == nil {
		err = issued.Write(out, "daemon")
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(out, "ca.crt"), ca.CertPEM(), 0o644)
	}
	if err != nil {
		fmt.Fprintln(stderr, "server issue-daemon-cert:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote daemon.crt, daemon.key and ca.crt to %s. Copy them to the daemon's machine and pass them as -cert, -key and -ca.\n", out)
	return 0
}
