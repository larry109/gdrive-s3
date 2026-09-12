// Command gdrive-s3 serves an S3-compatible API backed by Google Drive.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gdrives3/internal/config"
	"gdrives3/internal/gdrive"
	"gdrives3/internal/s3"
	"gdrives3/internal/storage"
)

func main() {
	log.SetFlags(0)
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	gd := gdrive.New(cfg.GoogleClientID, cfg.GoogleClientSecret, cfg.GoogleRefreshToken)
	store := storage.New(gd, cfg.RootFolder)

	creds := func(accessKey string) (string, bool) {
		if subtle.ConstantTimeCompare([]byte(accessKey), []byte(cfg.AccessKey)) == 1 {
			return cfg.SecretKey, true
		}
		return "", false
	}
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           s3.New(store, creds, cfg.Region).Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("gdrive-s3 listening on %s (root folder %q)", cfg.ListenAddr, cfg.RootFolder)
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
