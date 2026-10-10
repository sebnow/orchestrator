// Package s3test is a fake bucket for tests of code that uses package s3.
// It checks each request's signature with s3.Verify and its body against
// the signed hash, and keeps objects in memory.
package s3test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sebnow/orchestrator/internal/s3"
)

// Region and Bucket are the fake's region and bucket.
const (
	Region = "test-region"
	Bucket = "test-bucket"
)

// Credentials are the only credentials the fake accepts. They are not
// secret.
var Credentials = s3.Credentials{AccessKey: "TESTACCESSKEY", SecretKey: "test/secret/key/not/a/secret"}

// Server is a fake bucket.
type Server struct {
	*httptest.Server
	// PageSize is how many keys a listing page holds, 1000 by default.
	PageSize int

	mu      sync.Mutex
	objects map[string][]byte
	// failing, when not 0, is the status every request is answered with.
	failing int
}

// NewServer starts a fake bucket that is closed when the test ends.
func NewServer(t testing.TB) *Server {
	s := &Server{PageSize: 1000, objects: make(map[string][]byte)}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// Config is the configuration of a client of the fake.
func (s *Server) Config() s3.Config {
	return s3.Config{Endpoint: s.URL, Region: Region, Bucket: Bucket, Credentials: Credentials}
}

// Client is a client of the fake.
func (s *Server) Client(t testing.TB) *s3.Client {
	client, err := s3.NewClient(s.Config(), s.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// Objects is a copy of the objects the fake holds, by key.
func (s *Server) Objects() map[string][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.objects)
}

// SetObject stores content as key.
func (s *Server) SetObject(key string, content []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = content
}

// Fail answers every request with status, until called with 0.
func (s *Server) Fail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing = status
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	xml.NewEncoder(w).Encode(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}{Code: code, Message: message})
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	if err := s3.Verify(r, Credentials, Region); err != nil {
		writeError(w, http.StatusForbidden, "SignatureDoesNotMatch", err.Error())
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	hash := sha256.Sum256(body)
	if got := hex.EncodeToString(hash[:]); got != r.Header.Get("X-Amz-Content-Sha256") {
		writeError(w, http.StatusBadRequest, "XAmzContentSHA256Mismatch", "the body's hash is "+got)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing != 0 {
		writeError(w, s.failing, "InternalError", "the fake was told to fail")
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != Bucket {
		writeError(w, http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist.")
		return
	}
	switch {
	case key == "" && r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		s.list(w, r)
	case key == "":
		writeError(w, http.StatusNotImplemented, "NotImplemented", r.Method+" of the bucket")
	case r.Method == http.MethodPut:
		s.objects[key] = body
	case r.Method == http.MethodGet:
		content, ok := s.objects[key]
		if !ok {
			writeError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
			return
		}
		w.Write(content)
	case r.Method == http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

// list answers a ListObjectsV2 request; its continuation token is the
// index of the page's first key.
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	var keys []string
	for key := range s.objects {
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	start := 0
	if token := r.URL.Query().Get("continuation-token"); token != "" {
		var err error
		if start, err = strconv.Atoi(token); err != nil || start < 0 || start > len(keys) {
			writeError(w, http.StatusBadRequest, "InvalidArgument", "bad continuation token")
			return
		}
	}
	end := min(start+s.PageSize, len(keys))
	type content struct {
		Key          string `xml:"Key"`
		Size         int    `xml:"Size"`
		LastModified string `xml:"LastModified"`
	}
	page := struct {
		XMLName               xml.Name  `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
		Name                  string    `xml:"Name"`
		Prefix                string    `xml:"Prefix"`
		KeyCount              int       `xml:"KeyCount"`
		IsTruncated           bool      `xml:"IsTruncated"`
		NextContinuationToken string    `xml:"NextContinuationToken,omitempty"`
		Contents              []content `xml:"Contents"`
	}{Name: Bucket, Prefix: prefix, KeyCount: end - start, IsTruncated: end < len(keys)}
	if page.IsTruncated {
		page.NextContinuationToken = strconv.Itoa(end)
	}
	for _, key := range keys[start:end] {
		page.Contents = append(page.Contents, content{Key: key, Size: len(s.objects[key]), LastModified: time.Now().UTC().Format(time.RFC3339)})
	}
	w.Header().Set("Content-Type", "application/xml")
	fmt.Fprint(w, xml.Header)
	xml.NewEncoder(w).Encode(page)
}
