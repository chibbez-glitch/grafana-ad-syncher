package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	// Embeds the IANA tz database in the binary, so DISPLAY_TIMEZONE resolves on
	// the bare alpine runtime image without installing the tzdata package.
	_ "time/tzdata"

	"grafana-ad-syncher/internal/config"
	"grafana-ad-syncher/internal/dockerlog"
	"grafana-ad-syncher/internal/entra"
	"grafana-ad-syncher/internal/grafana"
	"grafana-ad-syncher/internal/logbuf"
	"grafana-ad-syncher/internal/store"
	syncer "grafana-ad-syncher/internal/sync"
	"grafana-ad-syncher/internal/web"
)

// apiTokenSettingKey is where an auto-generated token is persisted, so it
// survives restarts and stays stable across redeploys of the same volume.
const apiTokenSettingKey = "api_token"

func main() {
	cfg := config.Load()

	// UTC timestamps so app log lines line up with the docker log timestamps
	// that /api/logs/docker returns. Install the ring buffer before anything
	// else logs, so the API can serve the whole startup sequence.
	log.SetFlags(log.LstdFlags | log.LUTC)
	logBuffer := logbuf.New(cfg.LogBufferLines).Install()

	// UI timestamps only. The log stays UTC so it lines up with docker logs and
	// with what Grafana and Graph return.
	if err := web.SetDisplayLocation(cfg.DisplayTimezone); err != nil {
		log.Printf("DISPLAY_TIMEZONE %q not resolvable, falling back to UTC: %v", cfg.DisplayTimezone, err)
	} else {
		log.Printf("ui timestamps rendered in %s (log stays UTC)", web.DisplayLocation())
	}

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("data dir: %v", err)
	}

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	apiToken, err := resolveAPIToken(st, cfg.APIToken)
	if err != nil {
		log.Fatalf("api token: %v", err)
	}

	if cfg.AutoSyncOnStartSet {
		if err := st.SetAutoSyncEnabled(cfg.AutoSyncOnStart); err != nil {
			log.Printf("AUTO_SYNC_ON_START: failed to apply value=%t: %v", cfg.AutoSyncOnStart, err)
		} else {
			log.Printf("AUTO_SYNC_ON_START applied: auto_sync_enabled=%t", cfg.AutoSyncOnStart)
		}
	}

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

	// Say out loud whether unattended syncing is possible at all. With
	// SYNC_INTERVAL=0 the loop below is never started, so the "Automatische
	// Updates" toggle in the UI sets a flag that nothing reads - which looks
	// exactly like a working auto-sync that simply has not fired yet.
	if cfg.SyncInterval > 0 {
		log.Printf("auto sync loop: enabled, interval=%s (runs once immediately, then every tick; applies the whole plan without review)", cfg.SyncInterval)
	} else {
		log.Printf("auto sync loop: DISABLED because SYNC_INTERVAL=0 - the UI toggle has no effect until this is set to a duration such as 15m")
	}

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
	web.SetAutoSyncInterval(cfg.SyncInterval)
	server.Register(mux)

	// Troubleshooting endpoints, on the same mux and therefore the same port as
	// the UI. Token-guarded; see resolveAPIToken.
	dockerClient := dockerlog.New(cfg.DockerSocket)
	web.NewLogsAPI(logBuffer, dockerClient, apiToken, cfg.ContainerName).Register(mux)
	if err := dockerClient.Available(); err != nil {
		log.Printf("log api: docker socket unavailable, /api/logs/docker will fail: %v", err)
	} else {
		log.Printf("log api: docker socket %s reachable", dockerClient.Socket())
	}

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

// resolveAPIToken decides which token guards /api/logs/*.
//
// API_TOKEN from the environment always wins, so the operator can pin a known
// value in /etc/grafana-ad-syncher.env. Otherwise we reuse the token persisted
// in the store, and only if there is none do we mint one — that way the
// endpoints are usable straight after a deploy without anyone having to
// configure a secret first, and the value does not change on every restart.
//
// The generated token is logged in full exactly once, on the start that
// created it. Later starts log only a prefix: the log is retrievable over the
// network via this very API, so a token that reappeared in it on every restart
// would be a standing giveaway to anyone who already has it.
func resolveAPIToken(st *store.Store, fromEnv string) (string, error) {
	if fromEnv != "" {
		log.Printf("log api: token taken from API_TOKEN (prefix %s)", tokenPrefix(fromEnv))
		return fromEnv, nil
	}

	stored, ok, err := st.GetSetting(apiTokenSettingKey)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", apiTokenSettingKey, err)
	}
	if ok && strings.TrimSpace(stored) != "" {
		token := strings.TrimSpace(stored)
		log.Printf("log api: token loaded from store (prefix %s); set API_TOKEN to pin a known value", tokenPrefix(token))
		return token, nil
	}

	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	token := hex.EncodeToString(buf)
	if err := st.SetSetting(apiTokenSettingKey, token); err != nil {
		return "", fmt.Errorf("persist %s: %w", apiTokenSettingKey, err)
	}
	log.Printf("log api: generated a new API token, shown here once: %s", token)
	log.Printf("log api: use it as `Authorization: Bearer <token>` against /api/logs/*, or pin your own via API_TOKEN")
	return token, nil
}

func tokenPrefix(token string) string {
	if len(token) <= 8 {
		return "********"
	}
	return token[:8] + "..."
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
