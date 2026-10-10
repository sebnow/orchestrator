package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// The examples of "Signature Calculations for the Authorization Header:
// Transferring Payload in a Single Chunk (AWS Signature Version 4)",
// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html,
// as archived at
// https://web.archive.org/web/20251208134526/https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html.
var (
	exampleCredentials = Credentials{AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	exampleTime        = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
)

const exampleRegion = "us-east-1"

// signedHeaders lists the lower-case names of req's signed headers, as
// sign chooses them.
func signedHeaders(req *http.Request) []string {
	signed := []string{"host"}
	for name := range req.Header {
		if name != "Authorization" {
			signed = append(signed, strings.ToLower(name))
		}
	}
	slices.Sort(signed)
	return signed
}

func TestGivenAWSPublishedExamplesWhenSignedThenTheCanonicalRequestStringToSignAndAuthorizationMatchExactly(t *testing.T) {
	for _, c := range []struct {
		name          string
		method, url   string
		header        map[string]string
		payload       string
		canonical     string
		stringToSign  string
		authorization string
	}{
		{
			name:   "GET Object",
			method: http.MethodGet, url: "https://examplebucket.s3.amazonaws.com/test.txt",
			header: map[string]string{"Range": "bytes=0-9"},
			canonical: "GET\n/test.txt\n\nhost:examplebucket.s3.amazonaws.com\nrange:bytes=0-9\n" +
				"x-amz-content-sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\n" +
				"x-amz-date:20130524T000000Z\n\nhost;range;x-amz-content-sha256;x-amz-date\n" +
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			stringToSign: "AWS4-HMAC-SHA256\n20130524T000000Z\n20130524/us-east-1/s3/aws4_request\n" +
				"7344ae5b7ee6c3e7e6b0fe0640412a37625d1fbfff95c48bbb2dc43964946972",
			authorization: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
				"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date," +
				"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
		},
		{
			name:   "PUT Object",
			method: http.MethodPut, url: "https://examplebucket.s3.amazonaws.com/test$file.text",
			header:  map[string]string{"Date": "Fri, 24 May 2013 00:00:00 GMT", "X-Amz-Storage-Class": "REDUCED_REDUNDANCY"},
			payload: "Welcome to Amazon S3.",
			canonical: "PUT\n/test%24file.text\n\ndate:Fri, 24 May 2013 00:00:00 GMT\nhost:examplebucket.s3.amazonaws.com\n" +
				"x-amz-content-sha256:44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072\n" +
				"x-amz-date:20130524T000000Z\nx-amz-storage-class:REDUCED_REDUNDANCY\n\n" +
				"date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class\n" +
				"44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072",
			stringToSign: "AWS4-HMAC-SHA256\n20130524T000000Z\n20130524/us-east-1/s3/aws4_request\n" +
				"9e0e90d9c76de8fa5b200d8c849cd5b8dc7a3be3951ddb7f6a76b4158342019d",
			authorization: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
				"SignedHeaders=date;host;x-amz-content-sha256;x-amz-date;x-amz-storage-class," +
				"Signature=98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd",
		},
		{
			name:   "Get Bucket (List Objects)",
			method: http.MethodGet, url: "https://examplebucket.s3.amazonaws.com/?max-keys=2&prefix=J",
			canonical: "GET\n/\nmax-keys=2&prefix=J\nhost:examplebucket.s3.amazonaws.com\n" +
				"x-amz-content-sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855\n" +
				"x-amz-date:20130524T000000Z\n\nhost;x-amz-content-sha256;x-amz-date\n" +
				"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			stringToSign: "AWS4-HMAC-SHA256\n20130524T000000Z\n20130524/us-east-1/s3/aws4_request\n" +
				"df57d21db20da04d7fa30298dd4488ba3a2b47ca3a489c74750e0f1e7df1b9b7",
			authorization: "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request," +
				"SignedHeaders=host;x-amz-content-sha256;x-amz-date," +
				"Signature=34b48302e7b5fa45bde8084f4b7868a86f0a534bc59db6670ed5711ef69dc6f7",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(c.method, c.url, strings.NewReader(c.payload))
			if err != nil {
				t.Fatal(err)
			}
			for name, value := range c.header {
				req.Header.Set(name, value)
			}
			sum := sha256.Sum256([]byte(c.payload))
			payloadHash := hex.EncodeToString(sum[:])

			exampleCredentials.sign(req, exampleRegion, payloadHash, exampleTime)

			canonical := canonicalRequest(req.Method, req.URL.Path, req.URL.Query(), req.Header, requestHost(req), signedHeaders(req), payloadHash)
			if canonical != c.canonical {
				t.Errorf("canonical request\n%s\nwant\n%s", canonical, c.canonical)
			}
			if got := stringToSign("20130524T000000Z", exampleRegion, canonical); got != c.stringToSign {
				t.Errorf("string to sign\n%s\nwant\n%s", got, c.stringToSign)
			}
			if got := req.Header.Get("Authorization"); got != c.authorization {
				t.Errorf("Authorization\n%s\nwant\n%s", got, c.authorization)
			}
		})
	}
}

func TestGivenEveryByteWhenURIEncodedThenOnlyUnreservedCharactersAndSlashesInPathsStayAsTheyAre(t *testing.T) {
	if got, want := uriEncode("a-Z.0_9~/ $:+%é", true), "a-Z.0_9~/%20%24%3A%2B%25%C3%A9"; got != want {
		t.Errorf("path: %q, want %q", got, want)
	}
	if got, want := uriEncode("orchestrator/x", false), "orchestrator%2Fx"; got != want {
		t.Errorf("query value: %q, want %q", got, want)
	}
}

// sentRequest is req as a server receives it.
func sentRequest(t *testing.T, req *http.Request) *http.Request {
	t.Helper()
	received := httptest.NewRequest(req.Method, req.URL.RequestURI(), nil)
	received.Host = requestHost(req)
	received.Header = req.Header.Clone()
	return received
}

func TestGivenSignedRequestWhenVerifiedThenItIsAcceptedUnlessAnythingSignedChanged(t *testing.T) {
	newSigned := func() *http.Request {
		req, err := http.NewRequest(http.MethodGet, "https://fsn1.example.com/bucket/orchestrator/server-2026-10-10T14:30:05Z.db", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.URL.RawPath = canonicalURI(req.URL.Path)
		exampleCredentials.sign(req, "fsn1", emptyHash, exampleTime)
		return req
	}
	if err := Verify(sentRequest(t, newSigned()), exampleCredentials, "fsn1"); err != nil {
		t.Fatalf("an unchanged request was refused: %v", err)
	}
	for name, change := range map[string]func(*http.Request){
		"path":   func(r *http.Request) { r.URL.Path += "x"; r.URL.RawPath = canonicalURI(r.URL.Path) },
		"host":   func(r *http.Request) { r.Host = "nbg1.example.com" },
		"date":   func(r *http.Request) { r.Header.Set("X-Amz-Date", "20130524T000001Z") },
		"hash":   func(r *http.Request) { r.Header.Set("X-Amz-Content-Sha256", strings.Repeat("0", 64)) },
		"query":  func(r *http.Request) { r.URL.RawQuery = "acl=" },
		"method": func(r *http.Request) { r.Method = http.MethodDelete },
		"path sent unencoded": func(r *http.Request) {
			r.URL.RawPath = ""
		},
	} {
		req := newSigned()
		change(req)
		if err := Verify(sentRequest(t, req), exampleCredentials, "fsn1"); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s changed: error %v, want %v", name, err, ErrBadSignature)
		}
	}
	other := Credentials{AccessKey: exampleCredentials.AccessKey, SecretKey: "another secret"}
	if err := Verify(sentRequest(t, newSigned()), other, "fsn1"); !errors.Is(err, ErrBadSignature) {
		t.Errorf("another secret key: error %v, want %v", err, ErrBadSignature)
	}
	if err := Verify(sentRequest(t, newSigned()), exampleCredentials, "nbg1"); !errors.Is(err, ErrBadSignature) {
		t.Errorf("another region: error %v, want %v", err, ErrBadSignature)
	}
}
