package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/sebnow/orchestrator/internal/s3"
	"github.com/sebnow/orchestrator/internal/server"
)

// bucketFlags name the S3-compatible bucket backups go to
// (docs/adr/2026-10-10-sqlite-backups.md).
type bucketFlags struct {
	endpoint, region, bucket, credentials *string
}

func addBucketFlags(flags *flag.FlagSet) bucketFlags {
	return bucketFlags{
		endpoint:    flags.String("backup-s3-endpoint", "", "base URL of the S3-compatible storage backups are uploaded to, such as https://fsn1.your-objectstorage.com; http only on a loopback IP address"),
		region:      flags.String("backup-s3-region", "", "the storage's region, such as fsn1 at Hetzner"),
		bucket:      flags.String("backup-s3-bucket", "", "the bucket backups are uploaded to"),
		credentials: flags.String("backup-s3-credentials", "", "file of access_key= and secret_key= lines for the bucket, readable by its owner only"),
	}
}

// client returns the bucket's client, or nil when no bucket flag is
// given. A bucket needs all four; otherwise it returns the exit status.
func (f bucketFlags) client(stderr io.Writer) (*s3.Client, int) {
	given := map[string]string{"-backup-s3-endpoint": *f.endpoint, "-backup-s3-region": *f.region,
		"-backup-s3-bucket": *f.bucket, "-backup-s3-credentials": *f.credentials}
	var missing []string
	for _, name := range []string{"-backup-s3-endpoint", "-backup-s3-region", "-backup-s3-bucket", "-backup-s3-credentials"} {
		if given[name] == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == len(given) {
		return nil, 0
	}
	if len(missing) > 0 {
		fmt.Fprintf(stderr, "server: uploading backups also needs %s\n", strings.Join(missing, ", "))
		return nil, 2
	}
	endpoint, err := url.Parse(*f.endpoint)
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && loopbackHost(endpoint.Hostname()))) {
		fmt.Fprintln(stderr, "server: -backup-s3-endpoint must be an https URL, or http on a loopback IP address")
		return nil, 2
	}
	creds, err := s3.LoadCredentials(*f.credentials)
	if err != nil {
		fmt.Fprintln(stderr, "server: -backup-s3-credentials:", err)
		return nil, 1
	}
	client, err := s3.NewClient(s3.Config{Endpoint: *f.endpoint, Region: *f.region, Bucket: *f.bucket, Credentials: creds}, nil)
	if err != nil {
		fmt.Fprintln(stderr, "server:", err)
		return nil, 2
	}
	return client, 0
}

func loopbackHost(host string) bool {
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

// restore replaces the server's database with a backup, a file or a key
// in the bucket, once the backup passes SQLite's integrity check. The
// server must be stopped first (docs/adr/2026-10-10-sqlite-backups.md).
func restore(args []string, stdout, stderr io.Writer) int {
	flags := subcommandFlags("restore", "-db FILE -from FILE_OR_KEY [-force] [-backup-s3-endpoint URL -backup-s3-region REGION -backup-s3-bucket BUCKET -backup-s3-credentials FILE]", stderr)
	dbPath := flags.String("db", "", "the server's SQLite database file to replace; stop the server first (required)")
	from := flags.String("from", "", "the backup: a file, or, when no such file exists and the bucket flags are given, the key of an object in the bucket, such as orchestrator/server-2026-10-10T14:30:05Z.db (required)")
	force := flags.Bool("force", false, "replace an existing database, and drop its write-ahead log")
	bucket := addBucketFlags(flags)
	if status, ok := parseSubcommand(flags, args, dbPath, from); !ok {
		return status
	}
	client, status := bucket.client(stderr)
	if status != 0 {
		return status
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "server restore:", err)
		return 1
	}
	if _, err := os.Lstat(*dbPath); err == nil && !*force {
		return fail(fmt.Errorf("%s exists; stop the server and give -force to replace it", *dbPath))
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		return fail(err)
	}
	temp, err := os.CreateTemp(filepath.Dir(*dbPath), "."+filepath.Base(*dbPath)+".restore-*.tmp")
	if err != nil {
		return fail(err)
	}
	tempPath := temp.Name()
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			os.Remove(tempPath + suffix)
		}
	}()
	ctx := context.Background()
	if err := fetchBackup(ctx, temp, *from, client); err != nil {
		temp.Close()
		return fail(err)
	}
	if err := temp.Close(); err != nil {
		return fail(err)
	}
	version, err := server.VerifyBackup(ctx, tempPath)
	if err != nil {
		return fail(err)
	}
	// The replaced database's write-ahead log and shared memory belong to
	// it; SQLite would apply them to the backup.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(*dbPath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail(err)
		}
	}
	if err := os.Rename(tempPath, *dbPath); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "Restored %s from %s, at schema version %d. Start the server to use it.\n", *dbPath, *from, version)
	return 0
}

// fetchBackup writes the backup from into dest: the file from, or when
// there is none and client is set, the object whose key is from.
func fetchBackup(ctx context.Context, dest *os.File, from string, client *s3.Client) error {
	source, err := os.Open(from)
	switch {
	case err == nil:
		defer source.Close()
	case errors.Is(err, fs.ErrNotExist) && client != nil:
		body, err := client.Get(ctx, from)
		if err != nil {
			return fmt.Errorf("download %s from bucket %s: %w", from, client.Bucket(), err)
		}
		defer body.Close()
		if _, err := io.Copy(dest, body); err != nil {
			return fmt.Errorf("download %s from bucket %s: %w", from, client.Bucket(), err)
		}
		return dest.Sync()
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("-from %s: no such file, and no bucket is given to download it from", from)
	default:
		return err
	}
	if _, err := io.Copy(dest, source); err != nil {
		return err
	}
	return dest.Sync()
}
