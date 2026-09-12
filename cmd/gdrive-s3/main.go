// Command gdrive-s3 serves an S3-compatible API backed by Google Drive, with
// per-user Google OAuth and optional at-rest encryption.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gdrives3/internal/account"
	"gdrives3/internal/auth"
	"gdrives3/internal/config"
	"gdrives3/internal/crypt"
	"gdrives3/internal/s3"
	"gdrives3/internal/users"
)

func main() {
	log.SetFlags(0)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	var cipher *crypt.Cipher
	if cfg.EncryptionPassphrase != "" {
		if cipher, err = crypt.NewCipher(cfg.EncryptionPassphrase); err != nil {
			log.Fatalf("encryption: %v", err)
		}
		log.Printf("at-rest encryption enabled")
	}

	us, err := users.Open(cfg.UsersFile)
	if err != nil {
		log.Fatalf("users: %v", err)
	}
	mgr := account.New(us, cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.RootFolder, cipher)

	if cfg.SeedAccessKey != "" && cfg.SeedRefreshToken != "" {
		if err := mgr.Seed(cfg.SeedAccessKey, cfg.SeedSecretKey, cfg.SeedRefreshToken); err != nil {
			log.Fatalf("seed account: %v", err)
		}
		log.Printf("seeded account %s from configuration", cfg.SeedAccessKey)
	}

	mux := http.NewServeMux()
	auth.New(mgr, cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.PublicURL).Routes(mux)
	mux.Handle("/", s3.New(mgr, cfg.Region).Handler())

	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux, ReadHeaderTimeout: 15 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("gdrive-s3 listening on %s  (sign in at %s/auth/login)", cfg.ListenAddr, cfg.PublicURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("server: %v", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
