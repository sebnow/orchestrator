package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	from := flags.String("from", "", "the backup: a file, or, when no such file exists and the bucket flags are given, the key of an object in the bucket, such as orchestrator/server-2026-10-10T14:30:05Z.db.gz; a gzipped backup is gunzipped (required)")
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
	version, err := replaceDatabase(context.Background(), *dbPath, *from, client)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "Restored %s from %s, at schema version %d. Start the server to use it.\n", *dbPath, *from, version)
	return 0
}

// restoreTimeout bounds restoring the database from the bucket at start.
const restoreTimeout = 30 * time.Minute

// restoreAtStart restores the database at dbPath from the newest copy in
// bucket under prefix when dbPath does not exist
// (docs/adr/2026-10-10-server-loss.md). A bucket that holds no copy
// leaves dbPath to be created empty, which it logs. A database that
// exists is left as it is. It refuses to restore beside a write-ahead log
// or journal that has lost its database, since SQLite would apply it to
// the restored one.
func restoreAtStart(ctx context.Context, log *slog.Logger, bucket *s3.Client, prefix, dbPath string) error {
	if _, err := os.Lstat(dbPath); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for _, suffix := range []string{"-wal", "-journal"} {
		if _, err := os.Lstat(dbPath + suffix); err == nil {
			return fmt.Errorf("%s exists without its database; move it away to restore from the bucket", dbPath+suffix)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, restoreTimeout)
	defer cancel()
	key, err := server.NewestUpload(ctx, bucket, prefix)
	if err != nil {
		return err
	}
	if key == "" {
		log.Info("no backup in the bucket; starting with an empty database", "bucket", bucket.Bucket(), "prefix", prefix, "db", dbPath)
		return nil
	}
	version, err := replaceDatabase(ctx, dbPath, key, bucket)
	if err != nil {
		return fmt.Errorf("restore %s: %w", key, err)
	}
	log.Info("restored the database from the bucket", "key", key, "schema_version", version, "db", dbPath)
	return nil
}

// replaceDatabase puts the backup from, a file or, when there is no such
// file and client is set, a key in the bucket, at dbPath once it passes
// SQLite's integrity check and has a schema version this server can
// open, and returns that version. It downloads or copies the backup next
// to dbPath, gunzipping it when it is gzipped, deletes the write-ahead
// log, shared memory and journal of a database it replaces, and marks
// the database restored before it puts it in place.
func replaceDatabase(ctx context.Context, dbPath, from string, client *s3.Client) (int, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return 0, err
	}
	temp, err := os.CreateTemp(filepath.Dir(dbPath), "."+filepath.Base(dbPath)+".restore-*.tmp")
	if err != nil {
		return 0, err
	}
	tempPath := temp.Name()
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			os.Remove(tempPath + suffix)
		}
	}()
	if err := fetchBackup(ctx, temp, from, client); err != nil {
		temp.Close()
		return 0, err
	}
	if err := temp.Close(); err != nil {
		return 0, err
	}
	version, err := server.VerifyBackup(ctx, tempPath)
	if err != nil {
		return 0, err
	}
	// The replaced database's write-ahead log and shared memory belong to
	// it; SQLite would apply them to the backup.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := os.Remove(dbPath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 0, err
		}
	}
	// The restored database starts an epoch of its own when the server
	// opens it, so that it issues no command under the backup's
	// (docs/adr/2026-10-10-server-loss.md).
	if err := server.MarkRestored(dbPath, from); err != nil {
		return 0, err
	}
	if err := os.Rename(tempPath, dbPath); err != nil {
		return 0, err
	}
	return version, nil
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
		if err := copyBackup(dest, body); err != nil {
			return fmt.Errorf("download %s from bucket %s: %w", from, client.Bucket(), err)
		}
		return dest.Sync()
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("-from %s: no such file, and no bucket is given to download it from", from)
	default:
		return err
	}
	if err := copyBackup(dest, source); err != nil {
		return err
	}
	return dest.Sync()
}

// gzipMagic starts every gzip stream (RFC 1952); a SQLite database
// starts with "SQLite format 3" instead.
var gzipMagic = []byte{0x1f, 0x8b}

// copyBackup writes the database src holds to dest: gunzipped when src is
// gzipped, as uploads are, and as it is otherwise, as local copies and
// the uploads of earlier servers are (docs/adr/2026-10-10-server-loss.md).
// A gzipped backup's checksum is checked at its end.
func copyBackup(dest io.Writer, src io.Reader) error {
	buffered := bufio.NewReader(src)
	start, err := buffered.Peek(len(gzipMagic))
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if !bytes.Equal(start, gzipMagic) {
		_, err := io.Copy(dest, buffered)
		return err
	}
	zr, err := gzip.NewReader(buffered)
	if err != nil {
		return fmt.Errorf("gunzip the backup: %w", err)
	}
	if _, err := io.Copy(dest, zr); err != nil {
		return fmt.Errorf("gunzip the backup: %w", err)
	}
	return zr.Close()
}
