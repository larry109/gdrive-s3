// Package account resolves S3 access keys to a per-user storage backend and
// onboards new users from a Google OAuth refresh token.
package account

import (
	"sync"
	"time"

	"gdrives3/internal/crypt"
	"gdrives3/internal/gdrive"
	"gdrives3/internal/storage"
	"gdrives3/internal/users"
)

type Manager struct {
	users        *users.Store
	clientID     string
	clientSecret string
	rootFolder   string
	cipher       *crypt.Cipher

	mu     sync.Mutex
	stores map[string]*storage.Store
}

func New(us *users.Store, clientID, clientSecret, rootFolder string, cipher *crypt.Cipher) *Manager {
	return &Manager{
		users: us, clientID: clientID, clientSecret: clientSecret,
		rootFolder: rootFolder, cipher: cipher, stores: map[string]*storage.Store{},
	}
}

// Lookup returns the secret key and storage backend for an access key.
func (m *Manager) Lookup(accessKey string) (string, *storage.Store, bool) {
	u, ok := m.users.Get(accessKey)
	if !ok {
		return "", nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.stores[accessKey]
	if st == nil {
		gd := gdrive.New(m.clientID, m.clientSecret, u.RefreshToken)
		st = storage.New(gd, m.rootFolder, m.cipher)
		m.stores[accessKey] = st
	}
	return u.SecretKey, st, true
}

// CreateUser stores a new user for a Google refresh token and returns the
// generated S3 credentials.
func (m *Manager) CreateUser(refreshToken, email string) (users.User, error) {
	ak, sk := users.NewKeyPair()
	u := users.User{AccessKey: ak, SecretKey: sk, RefreshToken: refreshToken, Email: email, Created: time.Now()}
	if err := m.users.Put(u); err != nil {
		return users.User{}, err
	}
	return u, nil
}

// Seed inserts a user with fixed credentials (used to expose a single account
// from configuration, e.g. for local testing).
func (m *Manager) Seed(accessKey, secretKey, refreshToken string) error {
	return m.users.Put(users.User{
		AccessKey: accessKey, SecretKey: secretKey, RefreshToken: refreshToken, Created: time.Now(),
	})
}
