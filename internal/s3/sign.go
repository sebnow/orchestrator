package s3

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// AWS Signature Version 4 for S3, with the payload's hash signed
// (https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html).

const (
	algorithm = "AWS4-HMAC-SHA256"
	service   = "s3"
	// timeFormat is the ISO 8601 basic format of x-amz-date.
	timeFormat = "20060102T150405Z"
	// emptyHash is the SHA-256 of an empty payload.
	emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// uriEncode encodes every byte of s but the unreserved characters
// A-Z, a-z, 0-9, '-', '.', '_' and '~', and '/' when keepSlash is set, as
// '%' and two upper-case hexadecimal digits, as SigV4's UriEncode does.
func uriEncode(s string, keepSlash bool) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	for idx := range len(s) {
		c := s[idx]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '.', c == '_', c == '~', c == '/' && keepSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xf])
		}
	}
	return b.String()
}

// canonicalURI is the encoded path of a request for path.
func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	return uriEncode(path, true)
}

// canonicalQuery is query with each name and value encoded, sorted by
// name and then value. The client sends its query in this form, so what
// it signs is what it sends.
func canonicalQuery(query url.Values) string {
	pairs := make([]string, 0, len(query))
	for name, values := range query {
		for _, value := range values {
			pairs = append(pairs, uriEncode(name, false)+"="+uriEncode(value, false))
		}
	}
	slices.Sort(pairs)
	return strings.Join(pairs, "&")
}

// canonicalRequest is the canonical form of a request with method, path
// and query, whose header and host are signed under the lower-case
// names in signed, sorted, and whose payload has payloadHash.
func canonicalRequest(method, path string, query url.Values, header http.Header, host string, signed []string, payloadHash string) string {
	var b strings.Builder
	b.WriteString(method + "\n" + canonicalURI(path) + "\n" + canonicalQuery(query) + "\n")
	for _, name := range signed {
		values := header.Values(name)
		if name == "host" {
			values = []string{host}
		}
		trimmed := make([]string, len(values))
		for idx, value := range values {
			trimmed[idx] = strings.Join(strings.Fields(value), " ")
		}
		b.WriteString(name + ":" + strings.Join(trimmed, ",") + "\n")
	}
	b.WriteString("\n" + strings.Join(signed, ";") + "\n" + payloadHash)
	return b.String()
}

// scope is the credential scope of a signature made on date, YYYYMMDD,
// for region.
func scope(date, region string) string {
	return date + "/" + region + "/" + service + "/aws4_request"
}

func stringToSign(stamp, region, canonical string) string {
	hash := sha256.Sum256([]byte(canonical))
	return algorithm + "\n" + stamp + "\n" + scope(stamp[:8], region) + "\n" + hex.EncodeToString(hash[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

// signature is the hex signature of toSign by secretKey for region on
// date, YYYYMMDD.
func signature(secretKey, date, region, toSign string) string {
	key := hmacSHA256([]byte("AWS4"+secretKey), date)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, service)
	key = hmacSHA256(key, "aws4_request")
	return hex.EncodeToString(hmacSHA256(key, toSign))
}

// requestHost is the host a request is sent to, and signed for.
func requestHost(req *http.Request) string {
	if req.Host != "" {
		return req.Host
	}
	return req.URL.Host
}

// sign sets req's x-amz-date to now, its x-amz-content-sha256 to
// payloadHash, the hex SHA-256 of its body, and its Authorization to the
// signature by creds for region. It signs the host and every header req
// carries, so a header set after signing is not covered.
func (creds Credentials) sign(req *http.Request, region, payloadHash string, now time.Time) {
	stamp := now.UTC().Format(timeFormat)
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	req.Header.Del("Authorization")
	signed := []string{"host"}
	for name := range req.Header {
		signed = append(signed, strings.ToLower(name))
	}
	slices.Sort(signed)
	canonical := canonicalRequest(req.Method, req.URL.Path, req.URL.Query(), req.Header, requestHost(req), signed, payloadHash)
	req.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s,SignedHeaders=%s,Signature=%s",
		algorithm, creds.AccessKey, scope(stamp[:8], region), strings.Join(signed, ";"),
		signature(creds.SecretKey, stamp[:8], region, stringToSign(stamp, region, canonical))))
}

// ErrBadSignature reports a request whose signature Verify does not
// accept.
var ErrBadSignature = errors.New("bad signature")

// Verify checks r's signature as a bucket would: that creds signed it
// for region, covering its host, x-amz-date and x-amz-content-sha256,
// and that its path was sent in the canonical encoding that was signed.
// It does not read the body; the caller compares its hash with
// x-amz-content-sha256. It is the bucket's side of what Client does, for
// a fake bucket.
func Verify(r *http.Request, creds Credentials, region string) error {
	fields, ok := strings.CutPrefix(r.Header.Get("Authorization"), algorithm+" ")
	if !ok {
		return fmt.Errorf("%w: Authorization is not %s", ErrBadSignature, algorithm)
	}
	parts := make(map[string]string)
	for field := range strings.SplitSeq(fields, ",") {
		name, value, _ := strings.Cut(strings.TrimSpace(field), "=")
		parts[name] = value
	}
	stamp := r.Header.Get("X-Amz-Date")
	if _, err := time.Parse(timeFormat, stamp); err != nil {
		return fmt.Errorf("%w: x-amz-date %q", ErrBadSignature, stamp)
	}
	if want := creds.AccessKey + "/" + scope(stamp[:8], region); parts["Credential"] != want {
		return fmt.Errorf("%w: credential %q, want %q", ErrBadSignature, parts["Credential"], want)
	}
	signed := strings.Split(parts["SignedHeaders"], ";")
	for _, name := range []string{"host", "x-amz-content-sha256", "x-amz-date"} {
		if !slices.Contains(signed, name) {
			return fmt.Errorf("%w: %s is not signed", ErrBadSignature, name)
		}
	}
	if !slices.IsSorted(signed) {
		return fmt.Errorf("%w: signed headers are not sorted", ErrBadSignature)
	}
	sentPath, _, _ := strings.Cut(r.RequestURI, "?")
	if sentPath != canonicalURI(r.URL.Path) {
		return fmt.Errorf("%w: path sent as %q, signed as %q", ErrBadSignature, sentPath, canonicalURI(r.URL.Path))
	}
	canonical := canonicalRequest(r.Method, r.URL.Path, r.URL.Query(), r.Header, r.Host, signed, r.Header.Get("X-Amz-Content-Sha256"))
	want := signature(creds.SecretKey, stamp[:8], region, stringToSign(stamp, region, canonical))
	if !hmac.Equal([]byte(parts["Signature"]), []byte(want)) {
		return fmt.Errorf("%w: signature does not match", ErrBadSignature)
	}
	return nil
}
