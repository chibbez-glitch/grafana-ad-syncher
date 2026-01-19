package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"grafana-ad-syncher/internal/config"
	"grafana-ad-syncher/internal/entra"
	"grafana-ad-syncher/internal/grafana"
	"grafana-ad-syncher/internal/store"
	syncer "grafana-ad-syncher/internal/sync"
	"grafana-ad-syncher/internal/web"
)

func main() {
	cfg := config.Load()
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	grafanaClient := grafana.New(cfg.GrafanaURL, cfg.GrafanaAdminUser, cfg.GrafanaAdminPassword, cfg.GrafanaAdminToken, cfg.GrafanaInsecureTLS)
	entraClient := entra.New(cfg.EntraTenantID, cfg.EntraClientID, cfg.EntraClientSecret, cfg.EntraAuthorityBaseURL, cfg.GraphAPIBaseURL)
	clientSyncer := syncer.New(st, grafanaClient, entraClient, cfg.DefaultUserRole, cfg.AllowCreateUsers, cfg.AllowRemoveMembers)

	if cfg.SyncInterval > 0 {
		go func() {
			ticker := time.NewTicker(cfg.SyncInterval)
			defer ticker.Stop()
			for {
				if err := clientSyncer.Run(); err != nil {
					log.Printf("scheduled sync failed: %v", err)
				}
				<-ticker.C
			}
		}()
	}

	mux := http.NewServeMux()
	server, err := web.New(st, clientSyncer, grafanaClient, entraClient, filepath.Join("web", "templates"))
	if err != nil {
		log.Fatalf("templates: %v", err)
	}
	server.Register(mux)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir(filepath.Join("web", "static")))))

	httpServer := &http.Server{
		Addr:         cfg.ListenAddr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-shutdown
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	log.Printf("sync service listening on %s", cfg.ListenAddr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
}
