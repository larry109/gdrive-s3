// Package config loads settings from environment variables, with an optional
// .env file in the working directory as a fallback.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

type Config struct {
	// Google OAuth application (required, used for the multi-user sign-in flow).
	GoogleClientID     string
	GoogleClientSecret string

	// Optional single-account seed: exposes one Google account through fixed S3
	// credentials without going through the OAuth flow (handy for local use).
	SeedRefreshToken string
	SeedAccessKey    string
	SeedSecretKey    string

	ListenAddr string
	PublicURL  string
	RootFolder string
	Region     string
	UsersFile  string

	// Optional read-only demo account exposed by the browser console to
	// anonymous visitors (the access key of an onboarded user).
	DemoAccessKey string

	// Optional at-rest encryption; when set, all object content is encrypted
	// before it reaches Google Drive.
	EncryptionPassphrase string
}

func Load() (*Config, error) {
	env := dotenv(".env")
	get := func(key string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return env[key]
	}
	def := func(v, d string) string {
		if v == "" {
			return d
		}
		return v
	}

	c := &Config{
		GoogleClientID:       get("GOOGLE_CLIENT_ID"),
		GoogleClientSecret:   get("GOOGLE_CLIENT_SECRET"),
		SeedRefreshToken:     get("GOOGLE_REFRESH_TOKEN"),
		SeedAccessKey:        get("S3_ACCESS_KEY"),
		SeedSecretKey:        get("S3_SECRET_KEY"),
		ListenAddr:           def(get("LISTEN_ADDR"), "127.0.0.1:9000"),
		RootFolder:           def(get("ROOT_FOLDER"), "gdrive-s3"),
		Region:               def(get("REGION"), "us-east-1"),
		UsersFile:            def(get("USERS_FILE"), "data/users.json"),
		DemoAccessKey:        get("DEMO_ACCESS_KEY"),
		EncryptionPassphrase: get("ENCRYPTION_PASSPHRASE"),
	}
	c.PublicURL = def(get("PUBLIC_URL"), "http://"+c.ListenAddr)

	var missing []string
	if c.GoogleClientID == "" {
		missing = append(missing, "GOOGLE_CLIENT_ID")
	}
	if c.GoogleClientSecret == "" {
		missing = append(missing, "GOOGLE_CLIENT_SECRET")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

func dotenv(path string) map[string]string {
	m := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return m
}
