package main

import (
	"flag"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"strings"

	"github.com/sebnow/orchestrator/internal/s3"
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
