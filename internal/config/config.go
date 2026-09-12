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
	GoogleClientID     string
	GoogleClientSecret string
	GoogleRefreshToken string

	AccessKey string
	SecretKey string

	ListenAddr string
	RootFolder string
	Region     string
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
		GoogleClientID:     get("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: get("GOOGLE_CLIENT_SECRET"),
		GoogleRefreshToken: get("GOOGLE_REFRESH_TOKEN"),
		AccessKey:          get("S3_ACCESS_KEY"),
		SecretKey:          get("S3_SECRET_KEY"),
		ListenAddr:         def(get("LISTEN_ADDR"), "127.0.0.1:9000"),
		RootFolder:         def(get("ROOT_FOLDER"), "gdrive-s3"),
		Region:             def(get("REGION"), "us-east-1"),
	}

	var missing []string
	for k, v := range map[string]string{
		"GOOGLE_CLIENT_ID":     c.GoogleClientID,
		"GOOGLE_CLIENT_SECRET": c.GoogleClientSecret,
		"GOOGLE_REFRESH_TOKEN": c.GoogleRefreshToken,
		"S3_ACCESS_KEY":        c.AccessKey,
		"S3_SECRET_KEY":        c.SecretKey,
	} {
		if v == "" {
			missing = append(missing, k)
		}
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
