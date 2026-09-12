// Package users is a small persistent store mapping S3 access keys to the Google
// account (refresh token) they belong to. It is backed by a JSON file.
package users

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type User struct {
	AccessKey    string    `json:"access_key"`
	SecretKey    string    `json:"secret_key"`
	RefreshToken string    `json:"refresh_token"`
	Email        string    `json:"email,omitempty"`
	Created      time.Time `json:"created"`
}

type Store struct {
	path string
	mu   sync.RWMutex
	byAK map[string]User
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, byAK: map[string]User{}}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var list []User
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, err
	}
	for _, u := range list {
		s.byAK[u.AccessKey] = u
	}
	return s, nil
}

func (s *Store) Get(accessKey string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.byAK[accessKey]
	return u, ok
}

// Put inserts or replaces a user and persists the store.
func (s *Store) Put(u User) error {
	s.mu.Lock()
	s.byAK[u.AccessKey] = u
	list := make([]User, 0, len(s.byAK))
	for _, v := range s.byAK {
		list = append(list, v)
	}
	s.mu.Unlock()
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// NewKeyPair returns a fresh S3-style access key / secret key pair.
func NewKeyPair() (accessKey, secretKey string) {
	return "GS3" + randToken(17), randToken(40)
}

func randToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	return strings.ToUpper(enc[:n])
}
