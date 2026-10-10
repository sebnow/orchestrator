// Package s3 is a client for the parts of the S3 API that database
// backups use: putting, getting, listing and deleting objects in one
// bucket of an S3-compatible object storage, such as Hetzner's
// (docs/adr/2026-10-10-sqlite-backups.md). Requests are addressed
// path-style, https://<endpoint>/<bucket>/<key>, and signed with AWS
// Signature Version 4 over the payload's hash.
//
// The secret key is a secret: it only keys the signature, and never
// appears in a request or an error.
package s3

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// maxListBytes bounds one page of a listing, at most 1000 keys.
const maxListBytes = 8 << 20

// maxErrorBytes bounds the body of an error response.
const maxErrorBytes = 64 << 10

// ErrNotFound reports a key or bucket that does not exist.
var ErrNotFound = errors.New("not found")

// Error is an error the bucket answered with. It wraps ErrNotFound when
// its status is 404.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

func (e *Error) Unwrap() error {
	if e.Status == http.StatusNotFound {
		return ErrNotFound
	}
	return nil
}

// Credentials are an access key and its secret key.
type Credentials struct {
	AccessKey string
	SecretKey string
}

// readSettings reads path, a file only its owner may read or write, of
// name=value lines; blank lines and lines starting with '#' are skipped,
// and the space around names and values is dropped.
func readSettings(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s has mode %v; only its owner may read it (chmod 600)", path, info.Mode().Perm())
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w", path, fs.ErrInvalid)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	settings := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, value, ok := strings.Cut(text, "=")
		if !ok {
			// The line is not quoted: it may be a secret.
			return nil, fmt.Errorf("%s: line %d is not name=value", path, line)
		}
		settings[strings.TrimSpace(name)] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return settings, nil
}

// LoadCredentials reads the access_key and secret_key lines of path, a
// file only its owner may read or write. Other lines are ignored.
func LoadCredentials(path string) (Credentials, error) {
	settings, err := readSettings(path)
	if err != nil {
		return Credentials{}, fmt.Errorf("load S3 credentials: %w", err)
	}
	creds := Credentials{AccessKey: settings["access_key"], SecretKey: settings["secret_key"]}
	if creds.AccessKey == "" || creds.SecretKey == "" {
		return Credentials{}, fmt.Errorf("load S3 credentials: %s needs access_key= and secret_key= lines", path)
	}
	return creds, nil
}

// Config is where a Client's bucket is and how it signs in.
type Config struct {
	// Endpoint is the storage's base URL, without a path, such as
	// https://fsn1.your-objectstorage.com.
	Endpoint string
	// Region is the signature's region, such as fsn1 at Hetzner.
	Region string
	Bucket string
	Credentials
}

// Client calls one bucket.
type Client struct {
	endpoint *url.URL
	region   string
	bucket   string
	creds    Credentials
	http     *http.Client
	now      func() time.Time
}

// NewClient calls the bucket config names through httpClient, or
// http.DefaultClient when it is nil.
func NewClient(config Config, httpClient *http.Client) (*Client, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.Host == "" ||
		strings.Trim(endpoint.Path, "/") != "" || endpoint.RawQuery != "" || endpoint.User != nil {
		return nil, fmt.Errorf("s3: endpoint %q is not an http or https URL without a path", config.Endpoint)
	}
	endpoint.Path = ""
	switch {
	case config.Region == "":
		return nil, errors.New("s3: no region")
	case config.Bucket == "" || strings.Contains(config.Bucket, "/"):
		return nil, fmt.Errorf("s3: bucket %q is not a bucket name", config.Bucket)
	case config.AccessKey == "" || config.SecretKey == "":
		return nil, errors.New("s3: no credentials")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{endpoint: endpoint, region: config.Region, bucket: config.Bucket, creds: config.Credentials, http: httpClient, now: time.Now}, nil
}

// Bucket is the name of the client's bucket.
func (c *Client) Bucket() string {
	return c.bucket
}

// request is a signed request for key, "" for the bucket, with query,
// whose body, of size bytes, has the hex SHA-256 payloadHash.
func (c *Client) request(ctx context.Context, method, key string, query url.Values, body io.Reader, size int64, payloadHash string) (*http.Request, error) {
	target := *c.endpoint
	target.Path = "/" + c.bucket
	if key != "" {
		target.Path += "/" + key
	}
	target.RawPath = canonicalURI(target.Path)
	target.RawQuery = canonicalQuery(query)
	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	c.creds.sign(req, c.region, payloadHash, c.now())
	return req, nil
}

// do sends req and returns its response when its status is want, or
// else the bucket's error.
func (c *Client) do(req *http.Request, want ...int) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	for _, status := range want {
		if resp.StatusCode == status {
			return resp, nil
		}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBytes))
	apiErr := &Error{Status: resp.StatusCode, Code: http.StatusText(resp.StatusCode)}
	var body struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(data, &body) == nil && body.Code != "" {
		apiErr.Code, apiErr.Message = body.Code, body.Message
	}
	return nil, apiErr
}

// Put stores the size bytes of body as key, replacing any object there.
// body is read twice: once for its hash, which the signature covers, and
// once to send it.
func (c *Client) Put(ctx context.Context, key string, body io.ReadSeeker, size int64) error {
	if key == "" {
		return errors.New("s3: put: no key")
	}
	hash := sha256.New()
	hashed, err := io.Copy(hash, io.LimitReader(body, size+1))
	if err != nil {
		return fmt.Errorf("s3: put %s: %w", key, err)
	}
	if hashed != size {
		return fmt.Errorf("s3: put %s: the body has %d bytes, not %d", key, hashed, size)
	}
	if _, err := body.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("s3: put %s: %w", key, err)
	}
	req, err := c.request(ctx, http.MethodPut, key, nil, io.NopCloser(io.LimitReader(body, size)), size, hex.EncodeToString(hash.Sum(nil)))
	if err != nil {
		return fmt.Errorf("s3: put %s: %w", key, err)
	}
	resp, err := c.do(req, http.StatusOK)
	if err != nil {
		return fmt.Errorf("s3: put %s: %w", key, err)
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBytes))
	resp.Body.Close()
	return nil
}

// Get returns the content of key, which the caller closes.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == "" {
		return nil, errors.New("s3: get: no key")
	}
	req, err := c.request(ctx, http.MethodGet, key, nil, nil, 0, emptyHash)
	if err != nil {
		return nil, fmt.Errorf("s3: get %s: %w", key, err)
	}
	resp, err := c.do(req, http.StatusOK)
	if err != nil {
		return nil, fmt.Errorf("s3: get %s: %w", key, err)
	}
	return resp.Body, nil
}

// Delete deletes key. Deleting a key that does not exist succeeds.
func (c *Client) Delete(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("s3: delete: no key")
	}
	req, err := c.request(ctx, http.MethodDelete, key, nil, nil, 0, emptyHash)
	if err != nil {
		return fmt.Errorf("s3: delete %s: %w", key, err)
	}
	resp, err := c.do(req, http.StatusNoContent, http.StatusOK)
	if err != nil {
		return fmt.Errorf("s3: delete %s: %w", key, err)
	}
	resp.Body.Close()
	return nil
}

// Object is an object a listing names.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// listPage is a page of a ListObjectsV2 response.
type listPage struct {
	Contents []struct {
		Key          string    `xml:"Key"`
		Size         int64     `xml:"Size"`
		LastModified time.Time `xml:"LastModified"`
	} `xml:"Contents"`
	IsTruncated           bool   `xml:"IsTruncated"`
	NextContinuationToken string `xml:"NextContinuationToken"`
}

// List returns every object whose key starts with prefix, in the
// bucket's order, which is by key, following the listing's pages.
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var objects []Object
	token := ""
	for {
		query := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			query.Set("continuation-token", token)
		}
		req, err := c.request(ctx, http.MethodGet, "", query, nil, 0, emptyHash)
		if err != nil {
			return nil, fmt.Errorf("s3: list %s: %w", prefix, err)
		}
		resp, err := c.do(req, http.StatusOK)
		if err != nil {
			return nil, fmt.Errorf("s3: list %s: %w", prefix, err)
		}
		var page listPage
		err = xml.NewDecoder(io.LimitReader(resp.Body, maxListBytes)).Decode(&page)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("s3: list %s: %w", prefix, err)
		}
		for _, content := range page.Contents {
			objects = append(objects, Object(content))
		}
		if !page.IsTruncated {
			return objects, nil
		}
		if page.NextContinuationToken == "" || page.NextContinuationToken == token {
			return nil, fmt.Errorf("s3: list %s: truncated page %s without a new continuation token", prefix, strconv.Quote(token))
		}
		token = page.NextContinuationToken
	}
}
