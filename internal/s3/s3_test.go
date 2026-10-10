package s3_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sebnow/orchestrator/internal/s3"
	"github.com/sebnow/orchestrator/internal/s3/s3test"
)

func TestGivenFakeBucketWhenObjectsArePutListedGotAndDeletedThenEachRoundTrips(t *testing.T) {
	bucket := s3test.NewServer(t)
	bucket.PageSize = 2
	client := bucket.Client(t)
	ctx := t.Context()
	keys := []string{"orchestrator/server-2026-10-10T14:30:05Z.db", "orchestrator/a b$+.db", "orchestrator/c", "other/d", "orchestrator/é"}
	for idx, key := range keys {
		content := bytes.Repeat([]byte{byte('a' + idx)}, idx*1000)
		if err := client.Put(ctx, key, bytes.NewReader(content), int64(len(content))); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}

	objects, err := client.List(ctx, "orchestrator/")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, object := range objects {
		listed = append(listed, object.Key)
		if want := int64(slices.Index(keys, object.Key) * 1000); object.Size != want {
			t.Errorf("%s: size %d, want %d", object.Key, object.Size, want)
		}
	}
	want := []string{"orchestrator/a b$+.db", "orchestrator/c", "orchestrator/server-2026-10-10T14:30:05Z.db", "orchestrator/é"}
	if !slices.Equal(listed, want) {
		t.Errorf("listed %q, want %q over pages of 2", listed, want)
	}

	body, err := client.Get(ctx, keys[2])
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(body)
	body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, bytes.Repeat([]byte{'c'}, 2000)) {
		t.Errorf("got %d bytes, not what was put", len(content))
	}

	if err := client.Delete(ctx, keys[0]); err != nil {
		t.Fatal(err)
	}
	if _, ok := bucket.Objects()[keys[0]]; ok {
		t.Errorf("%s was not deleted", keys[0])
	}
	if _, err := client.Get(ctx, keys[0]); !errors.Is(err, s3.ErrNotFound) {
		t.Errorf("get of a deleted key: error %v, want %v", err, s3.ErrNotFound)
	}
}

func TestGivenWrongSecretKeyWhenAnObjectIsPutThenTheBucketRefusesAndTheErrorHoldsNoSecret(t *testing.T) {
	bucket := s3test.NewServer(t)
	config := bucket.Config()
	config.SecretKey = "wrong-secret-not-a-secret"
	client, err := s3.NewClient(config, bucket.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.Put(t.Context(), "k", strings.NewReader("x"), 1)
	var apiErr *s3.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden || apiErr.Code != "SignatureDoesNotMatch" {
		t.Fatalf("error %v, want 403 SignatureDoesNotMatch", err)
	}
	if strings.Contains(err.Error(), config.SecretKey) || strings.Contains(err.Error(), s3test.Credentials.SecretKey) {
		t.Errorf("error %q holds a secret key", err)
	}
	if len(bucket.Objects()) != 0 {
		t.Errorf("the bucket stored %v", bucket.Objects())
	}
}

func TestGivenBodyOfAnotherSizeWhenPutThenNothingIsSent(t *testing.T) {
	bucket := s3test.NewServer(t)
	client := bucket.Client(t)
	for _, size := range []int64{3, 5} {
		if err := client.Put(t.Context(), "k", strings.NewReader("four"), size); err == nil {
			t.Errorf("size %d of a 4-byte body was accepted", size)
		}
	}
	if len(bucket.Objects()) != 0 {
		t.Errorf("the bucket stored %v", bucket.Objects())
	}
}

func TestGivenFailingBucketWhenListedThenTheBucketsErrorIsReturned(t *testing.T) {
	bucket := s3test.NewServer(t)
	bucket.Fail(http.StatusServiceUnavailable)
	_, err := bucket.Client(t).List(t.Context(), "")
	var apiErr *s3.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Errorf("error %v, want 503", err)
	}
}

func TestGivenConfigWhenAClientIsMadeThenEndpointsWithAPathAndMissingSettingsAreRefused(t *testing.T) {
	good := s3.Config{Endpoint: "https://fsn1.your-objectstorage.com", Region: "fsn1", Bucket: "b", Credentials: s3.Credentials{AccessKey: "a", SecretKey: "s"}}
	if _, err := s3.NewClient(good, nil); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*s3.Config){
		"ftp endpoint":       func(c *s3.Config) { c.Endpoint = "ftp://fsn1.your-objectstorage.com" },
		"endpoint with path": func(c *s3.Config) { c.Endpoint = "https://fsn1.your-objectstorage.com/b" },
		"no region":          func(c *s3.Config) { c.Region = "" },
		"no bucket":          func(c *s3.Config) { c.Bucket = "" },
		"bucket with slash":  func(c *s3.Config) { c.Bucket = "b/c" },
		"no secret key":      func(c *s3.Config) { c.SecretKey = "" },
	} {
		config := good
		change(&config)
		if _, err := s3.NewClient(config, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func writeFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGivenCredentialsFileWhenLoadedThenItsKeysAreReadAndOtherLinesIgnored(t *testing.T) {
	path := writeFile(t, "# Hetzner, project default\n\naccess_key = AK\nsecret_key=S/K=\nendpoint=https://fsn1.your-objectstorage.com\n", 0o600)
	creds, err := s3.LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if creds != (s3.Credentials{AccessKey: "AK", SecretKey: "S/K="}) {
		t.Errorf("credentials %+v", creds)
	}
}

func TestGivenCredentialsFileOthersCanReadOrMissingAKeyWhenLoadedThenItIsRefusedWithoutShowingItsLines(t *testing.T) {
	for _, c := range []struct {
		content string
		mode    os.FileMode
		want    string
	}{
		{"access_key=AK\nsecret_key=hidden-secret\n", 0o640, "chmod 600"},
		{"access_key=AK\nsecret_key=hidden-secret\n", 0o604, "chmod 600"},
		{"access_key=AK\n", 0o600, "secret_key="},
		{"access_key=AK\nhidden-secret\n", 0o600, "line 2"},
	} {
		_, err := s3.LoadCredentials(writeFile(t, c.content, c.mode))
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), "hidden-secret") {
			t.Errorf("%q, mode %v: error %v, want one naming %q without the secret", c.content, c.mode, err, c.want)
		}
	}
}
