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
	ManageOrgRoles        bool
	// ReviewExcludeUsers are Grafana logins or e-mails kept out of the "Accounts
	// to review" panel: service and break-glass accounts that legitimately have no
	// Entra person behind them and would otherwise sit there for ever.
	ReviewExcludeUsers    []string
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

	// APIToken guards the /api/logs/* troubleshooting endpoints. Left empty
	// here, main generates one on first start and persists it in the store, so
	// the endpoints are never accidentally unauthenticated.
	APIToken string
	// LogBufferLines is how many recent log lines are kept in memory for
	// /api/logs/app. They cost roughly 200 bytes each.
	LogBufferLines int
	// DockerSocket is the daemon socket used by /api/logs/docker. It only works
	// if it is bind-mounted into this container.
	DockerSocket string
	// DisplayTimezone is the IANA zone the UI renders timestamps in. Storage and
	// logging stay UTC regardless.
	DisplayTimezone string
	// ContainerName is what /api/logs/docker reads when no ?container= is given.
	ContainerName string
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
		ManageOrgRoles:        getEnvBool("MANAGE_ORG_ROLES", true),
		ReviewExcludeUsers:    getEnvList("REVIEW_EXCLUDE_USERS", "admin"),
		EntraTenantID:         getEnv("ENTRA_TENANT_ID", ""),
		EntraClientID:         getEnv("ENTRA_CLIENT_ID", ""),
		EntraClientSecret:     getEnv("ENTRA_CLIENT_SECRET", ""),
		EntraAuthorityBaseURL: getEnv("ENTRA_AUTHORITY_BASE_URL", "https://login.microsoftonline.com"),
		GraphAPIBaseURL:       getEnv("GRAPH_API_BASE_URL", "https://graph.microsoft.com/v1.0"),
		APIToken:              strings.TrimSpace(getEnv("API_TOKEN", "")),
		LogBufferLines:        getEnvInt("LOG_BUFFER_LINES", 5000),
		DockerSocket:          getEnv("DOCKER_SOCKET", "/var/run/docker.sock"),
		ContainerName:         getEnv("CONTAINER_NAME", "grafana-sync"),
		DisplayTimezone:       getEnv("DISPLAY_TIMEZONE", "Europe/Luxembourg"),
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

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err == nil && parsed > 0 {
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

// getEnvList splits a comma-separated value into trimmed, lower-cased entries.
// Empty entries are dropped so a trailing comma is harmless.
func getEnvList(key, fallback string) []string {
	raw := getEnv(key, fallback)
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
