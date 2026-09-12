package s3

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// verifySigV4 authenticates an AWS Signature Version 4 (header form) request and
// returns the access key it was signed with. secretFor resolves the secret for
// an access key.
func verifySigV4(r *http.Request, secretFor func(string) (string, bool)) (string, *APIError) {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return "", &errMissingAuth
	}
	var credential, signedHeaders, signature string
	for _, part := range strings.Split(auth[len("AWS4-HMAC-SHA256 "):], ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "Credential="):
			credential = part[len("Credential="):]
		case strings.HasPrefix(part, "SignedHeaders="):
			signedHeaders = part[len("SignedHeaders="):]
		case strings.HasPrefix(part, "Signature="):
			signature = part[len("Signature="):]
		}
	}
	cp := strings.Split(credential, "/")
	if len(cp) != 5 || signedHeaders == "" || signature == "" {
		return "", &errAccessDenied
	}
	accessKey, date, region, service := cp[0], cp[1], cp[2], cp[3]
	secret, ok := secretFor(accessKey)
	if !ok {
		return "", &errInvalidAccessKey
	}
	amzDate := r.Header.Get("x-amz-date")
	if amzDate == "" {
		amzDate = r.Header.Get("X-Amz-Date")
	}
	payloadHash := r.Header.Get("x-amz-content-sha256")
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	}

	canonicalReq := strings.Join([]string{
		r.Method,
		canonicalURI(r.URL.Path),
		canonicalQuery(r),
		canonicalHeaders(r, signedHeaders),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{date, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hexSHA256([]byte(canonicalReq)),
	}, "\n")

	key := signingKey(secret, date, region, service)
	want := hex.EncodeToString(hmacSHA256(key, []byte(stringToSign)))
	if subtle.ConstantTimeCompare([]byte(want), []byte(signature)) != 1 {
		return "", &errSignatureMismatch
	}
	return accessKey, nil
}

func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s, false)
	}
	return strings.Join(segs, "/")
}

func canonicalQuery(r *http.Request) string {
	q := r.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		vals := q[k]
		sort.Strings(vals)
		for j, v := range vals {
			if i > 0 || j > 0 {
				b.WriteByte('&')
			}
			b.WriteString(uriEncode(k, true))
			b.WriteByte('=')
			b.WriteString(uriEncode(v, true))
		}
	}
	return b.String()
}

func canonicalHeaders(r *http.Request, signed string) string {
	var b strings.Builder
	for _, name := range strings.Split(signed, ";") {
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(collapse(headerValue(r, name)))
		b.WriteByte('\n')
	}
	return b.String()
}

func headerValue(r *http.Request, name string) string {
	if name == "host" {
		return r.Host
	}
	return r.Header.Get(name)
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// uriEncode implements the AWS SigV4 percent-encoding rules.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func hexSHA256(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte(service))
	return hmacSHA256(k, []byte("aws4_request"))
}

// bodyReader returns the object payload, transparently decoding the aws-chunked
// framing that the AWS CLI uses for streaming uploads, and the declared size.
func bodyReader(r *http.Request) (io.Reader, int64) {
	sha := r.Header.Get("x-amz-content-sha256")
	chunked := strings.HasPrefix(sha, "STREAMING") ||
		strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked")
	if !chunked {
		return r.Body, r.ContentLength
	}
	size := r.ContentLength
	if v := r.Header.Get("x-amz-decoded-content-length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			size = n
		}
	}
	return &chunkedReader{br: bufio.NewReader(r.Body)}, size
}

// chunkedReader strips aws-chunked framing (chunk sizes and per-chunk
// signatures) to yield the raw object bytes.
type chunkedReader struct {
	br  *bufio.Reader
	rem int64
	eof bool
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.eof {
		return 0, io.EOF
	}
	if c.rem == 0 {
		line, err := c.br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimRight(line, "\r\n")
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			c.eof = true
			return 0, io.EOF
		}
		c.rem = n
	}
	if int64(len(p)) > c.rem {
		p = p[:c.rem]
	}
	n, err := c.br.Read(p)
	c.rem -= int64(n)
	if c.rem == 0 && err == nil {
		// consume the trailing CRLF after the chunk data
		_, _ = c.br.Discard(2)
	}
	return n, err
}
