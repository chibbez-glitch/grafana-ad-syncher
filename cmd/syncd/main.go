package main

import (
	"bufio"
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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

	grafanaClient := grafana.New(cfg.GrafanaURL, cfg.GrafanaAdminUser, cfg.GrafanaAdminPassword, cfg.GrafanaAdminToken, cfg.GrafanaInsecureTLS, cfg.GrafanaDebug)
	entraClient := entra.New(cfg.EntraTenantID, cfg.EntraClientID, cfg.EntraClientSecret, cfg.EntraAuthorityBaseURL, cfg.GraphAPIBaseURL)

	if cfg.GrafanaDebug {
		log.Printf("grafana debug logging enabled (GRAFANA_DEBUG=true)")
		log.Printf("grafana config: url=%s insecureTLS=%t admin_user_set=%t admin_token_set=%t",
			cfg.GrafanaURL, cfg.GrafanaInsecureTLS, cfg.GrafanaAdminUser != "", cfg.GrafanaAdminToken != "")
		logEtcHosts()
		probeCtx, probeCancel := context.WithTimeout(context.Background(), 30*time.Second)
		grafanaClient.LogProbe(grafanaClient.Probe(probeCtx))
		probeCancel()
	}
	clientSyncer := syncer.New(st, grafanaClient, entraClient, cfg.DefaultUserRole, cfg.AllowCreateUsers, cfg.AllowRemoveMembers)

	if cfg.SyncInterval > 0 {
		go func() {
			ticker := time.NewTicker(cfg.SyncInterval)
			defer ticker.Stop()
			for {
				enabled, err := st.AutoSyncEnabled()
				if err != nil {
					log.Printf("auto sync status lookup failed: %v", err)
				} else if enabled {
					if err := clientSyncer.Run(); err != nil {
						log.Printf("scheduled sync failed: %v", err)
					}
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
		WriteTimeout: 2 * time.Minute,
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

// logEtcHosts prints the contents of /etc/hosts so we can verify whether the
// docker `extra_hosts` entries are actually visible inside the container.
// Lines starting with `#` and blank lines are skipped to keep the log compact.
func logEtcHosts() {
	f, err := os.Open("/etc/hosts")
	if err != nil {
		log.Printf("grafana probe: /etc/hosts unreadable: %v", err)
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	count := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		log.Printf("grafana probe: /etc/hosts %s", line)
		count++
	}
	if err := scanner.Err(); err != nil {
		log.Printf("grafana probe: /etc/hosts read error: %v", err)
	}
	if count == 0 {
		log.Printf("grafana probe: /etc/hosts contained no non-comment entries")
	}
}
