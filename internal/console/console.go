// Package console serves a browser UI and a small session-authenticated JSON API
// on top of the same storage backend as the S3 protocol layer. Signed-in users
// (via Google OAuth) get read-write access to their own Drive; anonymous callers
// fall back to an optional read-only demo account.
package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	_ "embed"

	"gdrives3/internal/storage"
)

//go:embed index.html
var indexHTML []byte

const cookieName = "gs3_session"

// Accounts resolves an access key to its per-user storage backend.
type Accounts interface {
	Lookup(accessKey string) (secret string, store *storage.Store, ok bool)
}

type session struct {
	accessKey string
	email     string
	expiry    time.Time
}

type Handler struct {
	accounts  Accounts
	demoKey   string
	endpoint  string
	region    string
	encrypted bool
	secure    bool

	mu       sync.Mutex
	sessions map[string]session

	limiter *limiter
}

func New(accounts Accounts, demoKey, endpoint, region string, encrypted bool) *Handler {
	return &Handler{
		accounts:  accounts,
		demoKey:   demoKey,
		endpoint:  strings.TrimRight(endpoint, "/"),
		region:    region,
		encrypted: encrypted,
		secure:    strings.HasPrefix(endpoint, "https://"),
		sessions:  map[string]session{},
		limiter:   newLimiter(5, 20),
	}
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/console/api/", h.api)
}

// ServeIndex writes the single-page UI. It is wired to the root path by the
// caller, which routes signed S3 requests elsewhere.
func (h *Handler) ServeIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(indexHTML)
}

// StartSession issues a session cookie bound to an access key. It is called by
// the OAuth handler once a user has signed in.
func (h *Handler) StartSession(w http.ResponseWriter, accessKey, email string) {
	tok := token()
	h.mu.Lock()
	h.sessions[tok] = session{accessKey: accessKey, email: email, expiry: time.Now().Add(12 * time.Hour)}
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", HttpOnly: true,
		Secure: h.secure, SameSite: http.SameSiteLaxMode, MaxAge: 12 * 3600,
	})
}

// resolve returns the storage backend for a request. A valid session cookie
// yields read-write access; otherwise the demo account (if configured) is used
// read-only.
func (h *Handler) resolve(r *http.Request) (store *storage.Store, email string, readOnly, ok bool) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		h.mu.Lock()
		s, found := h.sessions[c.Value]
		if found && time.Now().After(s.expiry) {
			delete(h.sessions, c.Value)
			found = false
		}
		h.mu.Unlock()
		if found {
			if _, st, ok2 := h.accounts.Lookup(s.accessKey); ok2 {
				return st, s.email, false, true
			}
		}
	}
	if h.demoKey != "" {
		if _, st, ok2 := h.accounts.Lookup(h.demoKey); ok2 {
			return st, "", true, true
		}
	}
	return nil, "", false, false
}

func (h *Handler) api(w http.ResponseWriter, r *http.Request) {
	if !h.limiter.allow(clientIP(r)) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return
	}
	p := strings.TrimPrefix(r.URL.Path, "/console/api/")
	store, email, readOnly, ok := h.resolve(r)

	if p == "session" {
		writeJSON(w, http.StatusOK, map[string]any{
			"authenticated": ok && !readOnly,
			"readOnly":      !ok || readOnly,
			"email":         email,
			"endpoint":      h.endpoint,
			"region":        h.region,
			"encrypted":     h.encrypted,
		})
		return
	}
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "not authenticated"})
		return
	}
	if readOnly && r.Method != http.MethodGet {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "read-only demo account"})
		return
	}

	switch {
	case p == "buckets" && r.Method == http.MethodGet:
		h.listBuckets(w, r, store)
	case p == "buckets" && r.Method == http.MethodPost:
		h.createBucket(w, r, store)
	case strings.HasPrefix(p, "buckets/"):
		h.bucketScoped(w, r, store, strings.TrimPrefix(p, "buckets/"))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (h *Handler) bucketScoped(w http.ResponseWriter, r *http.Request, store *storage.Store, rest string) {
	head, tail, _ := strings.Cut(rest, "/")
	bucket, err := url.PathUnescape(head)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid bucket"})
		return
	}
	switch {
	case tail == "" && r.Method == http.MethodDelete:
		h.mapErr(w, store.DeleteBucket(r.Context(), bucket))
	case tail == "objects" && r.Method == http.MethodGet:
		h.listObjects(w, r, store, bucket)
	case strings.HasPrefix(tail, "o/"):
		key, err := url.PathUnescape(strings.TrimPrefix(tail, "o/"))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid key"})
			return
		}
		h.object(w, r, store, bucket, key)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (h *Handler) listBuckets(w http.ResponseWriter, r *http.Request, store *storage.Store) {
	bs, err := store.ListBuckets(r.Context())
	if err != nil {
		h.mapErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(bs))
	for _, b := range bs {
		out = append(out, map[string]any{"name": b.Name, "created": b.Created})
	}
	writeJSON(w, http.StatusOK, map[string]any{"buckets": out})
}

func (h *Handler) createBucket(w http.ResponseWriter, r *http.Request, store *storage.Store) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bucket name required"})
		return
	}
	h.mapErr(w, store.CreateBucket(r.Context(), body.Name))
}

func (h *Handler) listObjects(w http.ResponseWriter, r *http.Request, store *storage.Store, bucket string) {
	q := r.URL.Query()
	res, err := store.ListObjects(r.Context(), bucket, q.Get("prefix"), q.Get("delimiter"), "", 1000)
	if err != nil {
		h.mapErr(w, err)
		return
	}
	objs := make([]map[string]any, 0, len(res.Objects))
	for _, o := range res.Objects {
		objs = append(objs, map[string]any{
			"key": o.Key, "size": o.Size, "etag": o.ETag, "lastModified": o.LastModified,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"objects": objs, "prefixes": res.CommonPrefixes,
	})
}

func (h *Handler) object(w http.ResponseWriter, r *http.Request, store *storage.Store, bucket, key string) {
	switch r.Method {
	case http.MethodGet:
		rc, obj, err := store.GetObject(r.Context(), bucket, key)
		if err != nil {
			h.mapErr(w, err)
			return
		}
		defer rc.Close()
		if obj.ContentType != "" {
			w.Header().Set("Content-Type", obj.ContentType)
		}
		w.Header().Set("Content-Length", itoa(obj.Size))
		name := key
		if i := strings.LastIndexByte(key, '/'); i >= 0 {
			name = key[i+1:]
		}
		w.Header().Set("Content-Disposition", "attachment; filename=\""+strings.ReplaceAll(name, `"`, "")+"\"")
		_, _ = io.Copy(w, rc)
	case http.MethodPut:
		ct := r.Header.Get("Content-Type")
		_, err := store.PutObject(r.Context(), bucket, key, r.ContentLength, r.Body, ct)
		h.mapErr(w, err)
	case http.MethodDelete:
		h.mapErr(w, store.DeleteObject(r.Context(), bucket, key))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (h *Handler) mapErr(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case errors.Is(err, storage.ErrNoSuchBucket):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such bucket"})
	case errors.Is(err, storage.ErrNoSuchKey):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such key"})
	case errors.Is(err, storage.ErrBucketExists):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "bucket already exists"})
	case errors.Is(err, storage.ErrBucketNotEmpty):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "bucket not empty"})
	case errors.Is(err, context.Canceled):
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func token() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// limiter is a per-client token bucket protecting the API on a public endpoint.
type limiter struct {
	rate, burst float64
	mu          sync.Mutex
	clients     map[string]*bucketState
}

type bucketState struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate, burst float64) *limiter {
	return &limiter{rate: rate, burst: burst, clients: map[string]*bucketState{}}
}

func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.clients[ip]
	if b == nil {
		if len(l.clients) > 10000 {
			l.clients = map[string]*bucketState{}
		}
		b = &bucketState{tokens: l.burst, last: now}
		l.clients[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
