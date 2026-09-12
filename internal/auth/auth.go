// Package auth implements the Google OAuth flow that onboards a user: they sign
// in with their Google account, the app stores the resulting refresh token, and
// the user receives an S3 access key / secret key pair scoped to their Drive.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gdrives3/internal/account"
)

const scope = "https://www.googleapis.com/auth/drive.file"

// Sessions issues a browser session cookie for an onboarded access key, so the
// user lands in the console already signed in after the OAuth flow.
type Sessions interface {
	StartSession(w http.ResponseWriter, accessKey, email string)
}

type Handler struct {
	mgr          *account.Manager
	sessions     Sessions
	clientID     string
	clientSecret string
	redirectURI  string

	mu     sync.Mutex
	states map[string]time.Time
}

func New(mgr *account.Manager, clientID, clientSecret, publicURL string, sessions Sessions) *Handler {
	return &Handler{
		mgr: mgr, sessions: sessions, clientID: clientID, clientSecret: clientSecret,
		redirectURI: strings.TrimRight(publicURL, "/") + "/auth/callback",
		states:      map[string]time.Time{},
	}
}

func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("/auth/login", h.login)
	mux.HandleFunc("/auth/callback", h.callback)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	state := h.newState()
	u := "https://accounts.google.com/o/oauth2/v2/auth?" + url.Values{
		"client_id":     {h.clientID},
		"redirect_uri":  {h.redirectURI},
		"response_type": {"code"},
		"scope":         {scope},
		"access_type":   {"offline"},
		"prompt":        {"consent"},
		"state":         {state},
	}.Encode()
	http.Redirect(w, r, u, http.StatusFound)
}

func (h *Handler) callback(w http.ResponseWriter, r *http.Request) {
	if !h.checkState(r.URL.Query().Get("state")) {
		http.Error(w, "invalid state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}
	refresh, err := h.exchange(r.Context(), code)
	if err != nil {
		http.Error(w, "token exchange failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	u, err := h.mgr.CreateUser(refresh, "")
	if err != nil {
		http.Error(w, "could not create user", http.StatusInternalServerError)
		return
	}
	if h.sessions != nil {
		h.sessions.StartSession(w, u.AccessKey, u.Email)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, credentialsPage, u.AccessKey, u.SecretKey)
}

func (h *Handler) exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"code":          {code},
		"client_id":     {h.clientID},
		"client_secret": {h.clientSecret},
		"redirect_uri":  {h.redirectURI},
		"grant_type":    {"authorization_code"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://oauth2.googleapis.com/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var tr struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", err
	}
	if tr.RefreshToken == "" {
		return "", fmt.Errorf("no refresh token returned (already authorized?)")
	}
	return tr.RefreshToken, nil
}

func (h *Handler) newState() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	s := hex.EncodeToString(b)
	h.mu.Lock()
	h.states[s] = time.Now().Add(10 * time.Minute)
	h.mu.Unlock()
	return s
}

func (h *Handler) checkState(s string) bool {
	if s == "" {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	exp, ok := h.states[s]
	delete(h.states, s)
	return ok && time.Now().Before(exp)
}

const credentialsPage = `<!doctype html><html><head><meta charset="utf-8"><title>gdrive-s3 credentials</title>
<style>body{font-family:system-ui;max-width:640px;margin:3rem auto;padding:0 1rem}
code{background:#f4f4f5;padding:.2rem .4rem;border-radius:4px}
.k{background:#f4f4f5;padding:1rem;border-radius:8px;margin:.5rem 0;font-family:monospace;word-break:break-all}</style></head>
<body><h1>Your S3 credentials</h1>
<p>Store these now &mdash; the secret key is shown only once.</p>
<p>Access key ID</p><div class="k">%s</div>
<p>Secret access key</p><div class="k">%s</div>
<p>Use them with any S3 client pointed at this server, e.g.:</p>
<pre>aws --endpoint-url $ENDPOINT s3 ls</pre>
<p><a href="/">Open the console &rarr;</a></p>
</body></html>`
