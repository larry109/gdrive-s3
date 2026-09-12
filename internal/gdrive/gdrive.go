// Package gdrive is a small client for the Google Drive v3 API scoped to
// drive.file. It handles OAuth token refresh and the folder/file operations the
// S3 layer needs.
package gdrive

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	tokenURL    = "https://oauth2.googleapis.com/token"
	filesURL    = "https://www.googleapis.com/drive/v3/files"
	uploadURL   = "https://www.googleapis.com/upload/drive/v3/files"
	folderMIME  = "application/vnd.google-apps.folder"
	defaultMIME = "application/octet-stream"
)

// ErrNotFound is returned when a file or folder does not exist.
var ErrNotFound = errors.New("gdrive: not found")

// File is a Drive resource (file or folder).
type File struct {
	ID       string
	Name     string
	MimeType string
	Size     int64
	MD5      string
	Modified time.Time
	IsFolder bool
}

// Client talks to the Google Drive API on behalf of a single account.
type Client struct {
	id, secret, refresh string
	hc                  *http.Client

	mu    sync.Mutex
	token string
	exp   time.Time
}

// New builds a client from OAuth credentials.
func New(clientID, clientSecret, refreshToken string) *Client {
	return &Client{id: clientID, secret: clientSecret, refresh: refreshToken, hc: &http.Client{}}
}

func (c *Client) token4(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.exp.Add(-time.Minute)) {
		return c.token, nil
	}
	form := url.Values{
		"client_id":     {c.id},
		"client_secret": {c.secret},
		"refresh_token": {c.refresh},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gdrive: token refresh: %s: %s", resp.Status, snippet(body))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		return "", err
	}
	c.token, c.exp = tr.AccessToken, time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second)
	return c.token, nil
}

func (c *Client) do(ctx context.Context, method, u string, body io.Reader) (*http.Request, error) {
	tok, err := c.token4(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	return req, nil
}

// Child returns the file named name directly under parent, optionally filtered
// to folders. Returns ErrNotFound if absent.
func (c *Client) Child(ctx context.Context, name, parent string, folderOnly bool) (*File, error) {
	q := fmt.Sprintf("name = '%s' and trashed = false and '%s' in parents", escape(name), parent)
	if folderOnly {
		q += " and mimeType = '" + folderMIME + "'"
	}
	files, err := c.query(ctx, q, 1)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, ErrNotFound
	}
	return &files[0], nil
}

// List returns the direct children of a folder.
func (c *Client) List(ctx context.Context, parent string) ([]File, error) {
	return c.query(ctx, fmt.Sprintf("'%s' in parents and trashed = false", parent), 1000)
}

func (c *Client) query(ctx context.Context, q string, pageSize int) ([]File, error) {
	u := filesURL + "?" + url.Values{
		"q":        {q},
		"fields":   {"files(id,name,mimeType,size,md5Checksum,modifiedTime)"},
		"pageSize": {strconv.Itoa(pageSize)},
		"spaces":   {"drive"},
	}.Encode()
	req, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gdrive: list: %s: %s", resp.Status, snippet(body))
	}
	var r struct {
		Files []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			MimeType string `json:"mimeType"`
			Size     string `json:"size"`
			MD5      string `json:"md5Checksum"`
			Modified string `json:"modifiedTime"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, err
	}
	out := make([]File, 0, len(r.Files))
	for _, f := range r.Files {
		size, _ := strconv.ParseInt(f.Size, 10, 64)
		mod, _ := time.Parse(time.RFC3339, f.Modified)
		out = append(out, File{
			ID: f.ID, Name: f.Name, MimeType: f.MimeType, Size: size, MD5: f.MD5,
			Modified: mod, IsFolder: f.MimeType == folderMIME,
		})
	}
	return out, nil
}

// CreateFolder creates a folder under parent (use "root" for My Drive root).
func (c *Client) CreateFolder(ctx context.Context, name, parent string) (*File, error) {
	meta := map[string]any{"name": name, "mimeType": folderMIME}
	if parent != "" {
		meta["parents"] = []string{parent}
	}
	mb, _ := json.Marshal(meta)
	req, err := c.do(ctx, http.MethodPost, filesURL+"?fields=id,name", bytes.NewReader(mb))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct{ ID, Name string }
	if err := c.json(req, http.StatusOK, &out); err != nil {
		return nil, err
	}
	return &File{ID: out.ID, Name: out.Name, IsFolder: true}, nil
}

// EnsureFolderPath resolves (creating if needed) segments under root and returns
// the leaf folder id.
func (c *Client) EnsureFolderPath(ctx context.Context, root string, segments []string) (string, error) {
	parent := root
	for _, seg := range segments {
		if seg == "" {
			continue
		}
		f, err := c.Child(ctx, seg, parent, true)
		switch {
		case errors.Is(err, ErrNotFound):
			nf, cerr := c.CreateFolder(ctx, seg, parent)
			if cerr != nil {
				return "", cerr
			}
			parent = nf.ID
		case err != nil:
			return "", err
		default:
			parent = f.ID
		}
	}
	return parent, nil
}

// Upload streams content as a new file under parent using a resumable session.
func (c *Client) Upload(ctx context.Context, name, parent string, size int64, r io.Reader, contentType string) (*File, error) {
	if contentType == "" {
		contentType = defaultMIME
	}
	meta := map[string]any{"name": name}
	if parent != "" {
		meta["parents"] = []string{parent}
	}
	mb, _ := json.Marshal(meta)
	req, err := c.do(ctx, http.MethodPost, uploadURL+"?uploadType=resumable&fields=id,name,size,md5Checksum,modifiedTime", bytes.NewReader(mb))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Upload-Content-Type", contentType)
	if size >= 0 {
		req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(size, 10))
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	session := resp.Header.Get("Location")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || session == "" {
		return nil, fmt.Errorf("gdrive: resumable init: %s", resp.Status)
	}

	put, err := http.NewRequestWithContext(ctx, http.MethodPut, session, r)
	if err != nil {
		return nil, err
	}
	put.ContentLength = size
	put.Header.Set("Content-Type", contentType)
	putResp, err := c.hc.Do(put)
	if err != nil {
		return nil, err
	}
	defer putResp.Body.Close()
	body, _ := io.ReadAll(putResp.Body)
	if putResp.StatusCode != http.StatusOK && putResp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("gdrive: upload: %s: %s", putResp.Status, snippet(body))
	}
	var raw struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Size     string `json:"size"`
		MD5      string `json:"md5Checksum"`
		Modified string `json:"modifiedTime"`
	}
	_ = json.Unmarshal(body, &raw)
	sz, _ := strconv.ParseInt(raw.Size, 10, 64)
	mod, _ := time.Parse(time.RFC3339, raw.Modified)
	return &File{ID: raw.ID, Name: raw.Name, Size: sz, MD5: raw.MD5, Modified: mod}, nil
}

// Download opens the content of a file.
func (c *Client) Download(ctx context.Context, id string) (io.ReadCloser, error) {
	req, err := c.do(ctx, http.MethodGet, filesURL+"/"+id+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("gdrive: download: %s: %s", resp.Status, snippet(body))
	}
	return resp.Body, nil
}

// Delete permanently removes a file or folder.
func (c *Client) Delete(ctx context.Context, id string) error {
	req, err := c.do(ctx, http.MethodDelete, filesURL+"/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gdrive: delete: %s: %s", resp.Status, snippet(body))
	}
	return nil
}

func (c *Client) json(req *http.Request, want int, v any) error {
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		return fmt.Errorf("gdrive: %s: %s", resp.Status, snippet(body))
	}
	if v == nil {
		return nil
	}
	return json.Unmarshal(body, v)
}

func escape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `'`, `\'`)
}

func snippet(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}
