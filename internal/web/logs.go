package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"grafana-ad-syncher/internal/dockerlog"
	"grafana-ad-syncher/internal/logbuf"
)

// LogsAPI exposes the log endpoints used for troubleshooting. It is mounted on
// the same mux, and therefore the same port, as the web UI.
//
// Everything here is read-only and every route is behind a bearer token: the
// app log is a verbatim copy of what goes to stderr (credentials are never
// logged, but Grafana/Entra request details are), and the docker routes are a
// window onto the daemon socket. Neither should be reachable without the token.
type LogsAPI struct {
	buf              *logbuf.Buffer
	docker           *dockerlog.Client
	token            string
	defaultContainer string
}

// NewLogsAPI wires the endpoints. defaultContainer is what /api/logs/docker
// reads when no ?container= is given — normally this container itself.
func NewLogsAPI(buf *logbuf.Buffer, docker *dockerlog.Client, token, defaultContainer string) *LogsAPI {
	return &LogsAPI{buf: buf, docker: docker, token: token, defaultContainer: defaultContainer}
}

func (a *LogsAPI) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/logs", a.guard(a.handleIndex))
	mux.HandleFunc("/api/logs/app", a.guard(a.handleApp))
	mux.HandleFunc("/api/logs/docker", a.guard(a.handleDocker))
	mux.HandleFunc("/api/logs/containers", a.guard(a.handleContainers))
}

// guard enforces the token on every log route.
//
// It deliberately answers 404 for an unconfigured token and 401 for a wrong
// one without naming which header was tried, and never logs the presented
// value — an unauthenticated caller should learn nothing beyond "no".
func (a *LogsAPI) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if a.token == "" {
			http.Error(w, "log API disabled: no API token configured", http.StatusNotFound)
			return
		}
		if subtle.ConstantTimeCompare([]byte(presentedToken(r)), []byte(a.token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="grafana-ad-syncher"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// presentedToken pulls the token out of whichever of the three accepted places
// the caller used. The query parameter is there so a plain browser tab or a
// bare curl works; the headers are the sane path.
func presentedToken(r *http.Request) string {
	if h := strings.TrimSpace(r.Header.Get("Authorization")); h != "" {
		// Accept a bare token too - it is a common curl slip and rejecting it
		// costs a round of confusion for no security gain.
		if len(h) >= 6 && strings.EqualFold(h[:6], "Bearer") {
			return strings.TrimSpace(h[6:])
		}
		return h
	}
	if h := r.Header.Get("X-API-Token"); h != "" {
		return strings.TrimSpace(h)
	}
	return strings.TrimSpace(r.URL.Query().Get("token"))
}

func (a *LogsAPI) handleIndex(w http.ResponseWriter, r *http.Request) {
	dockerErr := ""
	if err := a.docker.Available(); err != nil {
		dockerErr = err.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"now":               time.Now().UTC().Format(time.RFC3339),
		"app_buffer_lines":  len(a.buf.Snapshot(0, time.Time{}, "")),
		"app_buffer_cap":    a.buf.Capacity(),
		"docker_socket":     a.docker.Socket(),
		"docker_available":  dockerErr == "",
		"docker_error":      dockerErr,
		"default_container": a.defaultContainer,
		"endpoints": []string{
			"GET /api/logs                 - this overview",
			"GET /api/logs/app             - in-memory app log; params: tail, since, q, format",
			"GET /api/logs/docker          - container log; params: container, tail, since, q, stream, format",
			"GET /api/logs/containers      - container list from the docker daemon",
		},
		"params": map[string]string{
			"tail":      "number of lines (app), number or 'all' (docker); default 200",
			"since":     "duration like 15m/2h, or an RFC3339 timestamp",
			"q":         "case-insensitive substring filter",
			"stream":    "docker only: stdout, stderr or both (default both)",
			"container": "docker only: container name or id; default " + a.defaultContainer,
			"format":    "text (default) or json",
		},
		"auth": "Authorization: Bearer <token>, X-API-Token: <token>, or ?token=<token>",
	})
}

func (a *LogsAPI) handleApp(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	since, err := parseSince(q.Get("since"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	lines := a.buf.Snapshot(parseTail(q.Get("tail"), 200), since, q.Get("q"))

	if strings.EqualFold(q.Get("format"), "json") {
		writeJSON(w, http.StatusOK, map[string]any{
			"source":   "app",
			"capacity": a.buf.Capacity(),
			"count":    len(lines),
			"lines":    lines,
		})
		return
	}
	writeText(w, len(lines), func(sb *strings.Builder) {
		for _, ln := range lines {
			sb.WriteString(ln.Text)
			sb.WriteByte('\n')
		}
	})
}

func (a *LogsAPI) handleDocker(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	name := strings.TrimSpace(q.Get("container"))
	if name == "" {
		name = a.defaultContainer
	}
	since, err := parseSince(q.Get("since"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tail := strings.TrimSpace(q.Get("tail"))
	if tail != "all" {
		tail = strconv.Itoa(parseTail(tail, 200))
	}

	// The daemon can be slow on a big tail; bound it by the request context so a
	// hung socket cannot pin the handler past the server write timeout.
	ctx, cancel := contextWithTimeout(r, 60*time.Second)
	defer cancel()

	lines, err := a.docker.Logs(ctx, name, tail, since)
	if err != nil {
		// Anything wrong here is a deployment problem (socket not mounted, no
		// permission, unknown container), so the message is the useful part.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"source":    "docker",
			"container": name,
			"error":     err.Error(),
			"socket":    a.docker.Socket(),
		})
		return
	}
	lines = filterDockerLines(lines, q.Get("stream"), q.Get("q"))

	if strings.EqualFold(q.Get("format"), "json") {
		writeJSON(w, http.StatusOK, map[string]any{
			"source":    "docker",
			"container": name,
			"count":     len(lines),
			"lines":     lines,
		})
		return
	}
	writeText(w, len(lines), func(sb *strings.Builder) {
		for _, ln := range lines {
			if !ln.At.IsZero() {
				sb.WriteString(ln.At.Format(time.RFC3339))
				sb.WriteByte(' ')
			}
			if ln.Stream == "stderr" {
				sb.WriteString("[stderr] ")
			}
			sb.WriteString(ln.Text)
			sb.WriteByte('\n')
		}
	})
}

func (a *LogsAPI) handleContainers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 15*time.Second)
	defer cancel()

	containers, err := a.docker.Containers(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":  err.Error(),
			"socket": a.docker.Socket(),
		})
		return
	}
	sort.Slice(containers, func(i, j int) bool {
		return strings.Join(containers[i].Names, ",") < strings.Join(containers[j].Names, ",")
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"count":      len(containers),
		"containers": containers,
	})
}

func filterDockerLines(lines []dockerlog.Line, stream, substr string) []dockerlog.Line {
	stream = strings.ToLower(strings.TrimSpace(stream))
	needle := strings.ToLower(strings.TrimSpace(substr))
	if (stream == "" || stream == "both") && needle == "" {
		return lines
	}
	out := make([]dockerlog.Line, 0, len(lines))
	for _, ln := range lines {
		// "unknown" comes from TTY containers where the streams are already
		// merged; filtering those out would silently return nothing.
		if stream != "" && stream != "both" && ln.Stream != stream && ln.Stream != "unknown" {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(ln.Text), needle) {
			continue
		}
		out = append(out, ln)
	}
	return out
}

// parseTail accepts a positive line count; anything else falls back to the
// default rather than erroring, so a typo still returns logs.
func parseTail(raw string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return fallback
	}
	if n > 20000 {
		return 20000
	}
	return n
}

// parseSince takes either a Go duration ("15m", "2h") meaning "that long ago",
// or an absolute RFC3339 timestamp. Empty means no lower bound.
func parseSince(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(raw); err == nil {
		if d < 0 {
			d = -d
		}
		return time.Now().UTC().Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid since=%q: use a duration like 15m/2h or an RFC3339 timestamp", raw)
}

// contextWithTimeout derives from the request context, so a client that hangs
// up also cancels the daemon call instead of leaving it running.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}

// writeText renders plain text, with the line count in a header so a caller
// can tell "no matches" apart from an empty body caused by a filter typo.
func writeText(w http.ResponseWriter, count int, render func(*strings.Builder)) {
	var sb strings.Builder
	render(&sb)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Log-Lines", strconv.Itoa(count))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(sb.String()))
}
