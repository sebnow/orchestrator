//go:build live

// The live test puts, lists, gets and deletes one small object in a real
// bucket, configured in ~/.config/orchestrator/backup-s3, a file only its
// owner may read, of endpoint=, region=, bucket=, access_key= and
// secret_key= lines; without that file it skips. The object is under
// orchestrator-live-test/ and is deleted again. Run with:
//
//	go test -tags live -run Live -v ./internal/s3/
package s3

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveGivenBucketWhenAnObjectIsPutListedGotAndDeletedThenEachRoundTripsAndNothingIsLeft(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config", "orchestrator", "backup-s3")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		t.Skipf("%s does not exist; no bucket, so no live test", path)
	}
	settings, err := readSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{Endpoint: settings["endpoint"], Region: settings["region"], Bucket: settings["bucket"], Credentials: creds}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("bucket %s at %s, region %s", settings["bucket"], settings["endpoint"], settings["region"])
	ctx := t.Context()
	prefix := "orchestrator-live-test/" + rand.Text() + "/"
	// The name is shaped like a backup's, colons included.
	key := prefix + "server-" + time.Now().UTC().Format(time.RFC3339) + ".db"
	content := []byte("orchestrator live test object\n")
	t.Cleanup(func() {
		// Deleting a key that is gone succeeds, so this is safe after the
		// test's own delete. The test's context has ended by now.
		if err := client.Delete(context.Background(), key); err != nil {
			t.Errorf("clean up %s: %v", key, err)
		}
	})

	if err := client.Put(ctx, key, bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	objects, err := client.List(ctx, prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || objects[0].Key != key || objects[0].Size != int64(len(content)) {
		t.Errorf("listed %+v, want only %s of %d bytes", objects, key, len(content))
	}
	body, err := client.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("got %q, want %q", got, content)
	}
	if err := client.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if objects, err := client.List(ctx, prefix); err != nil || len(objects) != 0 {
		t.Errorf("after delete: listed %+v, %v; want nothing", objects, err)
	}
	if _, err := client.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: error %v, want %v", err, ErrNotFound)
	}
}
