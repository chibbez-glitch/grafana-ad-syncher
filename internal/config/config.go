package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr           string
	DataDir              string
	SyncInterval         time.Duration
	GrafanaURL            string
	GrafanaAdminUser      string
	GrafanaAdminPassword  string
	GrafanaAdminToken     string
	GrafanaInsecureTLS    bool
	GrafanaDebug          bool
	DefaultUserRole       string
	AllowCreateUsers      bool
	AllowRemoveMembers    bool
	EntraTenantID         string
	EntraClientID         string
	EntraClientSecret     string
	EntraAuthorityBaseURL string
	GraphAPIBaseURL       string

	// AutoSyncOnStart, when AutoSyncOnStartSet is true, forces the store's
	// auto-sync flag to that value on every container start. When unset, the
	// existing store value (toggled via the web UI) is left alone.
	AutoSyncOnStart    bool
	AutoSyncOnStartSet bool
}

func Load() Config {
	cfg := Config{
		ListenAddr:           getEnv("LISTEN_ADDR", ":8080"),
		DataDir:              getEnv("DATA_DIR", "/data"),
		SyncInterval:         getEnvDuration("SYNC_INTERVAL", 15*time.Minute),
		GrafanaURL:            getEnv("GRAFANA_URL", "http://grafana:3000"),
		GrafanaAdminUser:      getEnv("GRAFANA_ADMIN_USER", "admin"),
		GrafanaAdminPassword:  getEnv("GRAFANA_ADMIN_PASSWORD", ""),
		GrafanaAdminToken:     getEnv("GRAFANA_ADMIN_TOKEN", ""),
		GrafanaInsecureTLS:    getEnvBool("GRAFANA_INSECURE_TLS", false),
		GrafanaDebug:          getEnvBool("GRAFANA_DEBUG", false),
		DefaultUserRole:       getEnv("DEFAULT_USER_ROLE", "Viewer"),
		AllowCreateUsers:      getEnvBool("ALLOW_CREATE_USERS", true),
		AllowRemoveMembers:    getEnvBool("ALLOW_REMOVE_TEAM_MEMBERS", true),
		EntraTenantID:         getEnv("ENTRA_TENANT_ID", ""),
		EntraClientID:         getEnv("ENTRA_CLIENT_ID", ""),
		EntraClientSecret:     getEnv("ENTRA_CLIENT_SECRET", ""),
		EntraAuthorityBaseURL: getEnv("ENTRA_AUTHORITY_BASE_URL", "https://login.microsoftonline.com"),
		GraphAPIBaseURL:       getEnv("GRAPH_API_BASE_URL", "https://graph.microsoft.com/v1.0"),
	}
	if raw, ok := os.LookupEnv("AUTO_SYNC_ON_START"); ok && strings.TrimSpace(raw) != "" {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(raw)); err == nil {
			cfg.AutoSyncOnStart = parsed
			cfg.AutoSyncOnStartSet = true
		}
	}
	return cfg
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		parsed, err := strconv.ParseBool(v)
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		parsed, err := time.ParseDuration(v)
		if err == nil {
			return parsed
		}
	}
	return fallback
}
