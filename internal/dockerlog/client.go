// Package dockerlog is a deliberately small read-only client for the Docker
// Engine API over its unix socket. It exists so the troubleshooting endpoints
// can show the logs of *neighbouring* containers (grafana, above all) without
// pulling in the full Docker SDK — we only need two GET calls.
package dockerlog

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultSocket is where the docker daemon socket lives on a standard Linux
// host. It has to be bind-mounted into this container for any of this to work.
const DefaultSocket = "/var/run/docker.sock"

// maxLogBytes caps how much we read from a single logs call. Without it a
// tail=all on a chatty container would pull the whole history into memory.
const maxLogBytes = 8 << 20 // 8 MiB

// nameRE is what we accept as a container name or id. The rules docker itself
// applies are looser, but the value goes into a URL path, so keep it boring.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Client talks to the daemon. The zero value is not usable; use New.
type Client struct {
	socket string
	http   *http.Client
}

// Container is the subset of the container listing from the daemon that we
// surface.
type Container struct {
	ID      string   `json:"id"`
	Names   []string `json:"names"`
	Image   string   `json:"image"`
	State   string   `json:"state"`
	Status  string   `json:"status"`
	Created int64    `json:"created"`
}

// Line is one demultiplexed log line. Stream is "stdout", "stderr" or
// "unknown" (the latter for TTY containers, where docker does not frame the
// output and the two streams are already merged).
type Line struct {
	Stream string    `json:"stream"`
	At     time.Time `json:"at,omitempty"`
	Text   string    `json:"text"`
}

// New returns a client for the given socket path, falling back to
// DefaultSocket when empty.
func New(socket string) *Client {
	if strings.TrimSpace(socket) == "" {
		socket = DefaultSocket
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &Client{
		socket: socket,
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

// Socket is the path this client dials, for error messages and diagnostics.
func (c *Client) Socket() string { return c.socket }

// Available reports whether the socket is present and reachable by this
// process. It separates "not mounted" from "mounted but no permission",
// because those need very different fixes and the difference is invisible from
// a generic connection error.
func (c *Client) Available() error {
	info, err := os.Stat(c.socket)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s does not exist inside the container - add it as a read-only bind mount in docker-compose.yml", c.socket)
	}
	if err != nil {
		return fmt.Errorf("cannot stat %s: %w", c.socket, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a socket (mode %s)", c.socket, info.Mode())
	}
	// Probe by connecting, not by opening. open(2) on a socket inode always
	// fails with ENXIO ("no such device or address") no matter who you are, so
	// an os.OpenFile check here reports every socket as unusable - including a
	// perfectly working one. connect(2) is the only way to learn anything.
	conn, err := net.DialTimeout("unix", c.socket, 5*time.Second)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%s is mounted but uid %d/gid %d may not connect to it - add the docker group gid of the host via group_add in docker-compose.yml: %w",
				c.socket, os.Getuid(), os.Getgid(), err)
		}
		return fmt.Errorf("cannot connect to %s: %w", c.socket, err)
	}
	_ = conn.Close()
	return nil
}

// Containers lists all containers, running or not.
func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	body, err := c.get(ctx, "/containers/json?all=1", 1<<20)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID      string   `json:"Id"`
		Names   []string `json:"Names"`
		Image   string   `json:"Image"`
		State   string   `json:"State"`
		Status  string   `json:"Status"`
		Created int64    `json:"Created"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode container list: %w", err)
	}
	out := make([]Container, 0, len(raw))
	for _, r := range raw {
		names := make([]string, 0, len(r.Names))
		for _, n := range r.Names {
			names = append(names, strings.TrimPrefix(n, "/"))
		}
		id := r.ID
		if len(id) > 12 {
			id = id[:12]
		}
		out = append(out, Container{ID: id, Names: names, Image: r.Image, State: r.State, Status: r.Status, Created: r.Created})
	}
	return out, nil
}

// Logs fetches stdout+stderr of one container. tail is a line count ("200") or
// "all"; since limits how far back to go and may be the zero time for "no
// limit".
func (c *Client) Logs(ctx context.Context, name string, tail string, since time.Time) ([]Line, error) {
	if !nameRE.MatchString(name) {
		return nil, fmt.Errorf("invalid container name %q", name)
	}
	if tail == "" {
		tail = "200"
	}
	q := url.Values{}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("timestamps", "1")
	q.Set("tail", tail)
	if !since.IsZero() {
		q.Set("since", strconv.FormatInt(since.Unix(), 10))
	}
	body, err := c.get(ctx, "/containers/"+name+"/logs?"+q.Encode(), maxLogBytes)
	if err != nil {
		return nil, err
	}
	return parseLogStream(body), nil
}

func (c *Client) get(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := c.Available(); err != nil {
		return nil, err
	}
	// The host part is ignored - the transport always dials the unix socket -
	// but net/http still insists on a syntactically valid URL.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker api %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("docker api %s: read body: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return nil, fmt.Errorf("docker api %s: status %d: %s", path, resp.StatusCode, msg)
	}
	return body, nil
}

// parseLogStream turns a raw /logs payload into lines.
//
// Docker multiplexes stdout and stderr into 8-byte-framed chunks unless the
// container was started with a TTY, in which case the payload is plain text.
// We try the framed layout first and fall back to raw on any inconsistency,
// which is the only way to tell the two apart without a separate inspect call.
func parseLogStream(data []byte) []Line {
	if frames, ok := demux(data); ok {
		return frames
	}
	return splitLines(string(data), "unknown")
}

// demux walks the frame headers. A malformed header at offset 0 means the
// payload was never framed to begin with (TTY container) and the caller should
// treat it as raw text. A malformed or short frame *after* at least one good
// one means we hit the maxLogBytes cut mid-frame, so we keep what parsed and
// drop the ragged tail rather than throwing the whole response away.
func demux(data []byte) ([]Line, bool) {
	var out []Line
	pos := 0
	for pos < len(data) {
		if len(data)-pos < 8 {
			break
		}
		hdr := data[pos : pos+8]
		if hdr[0] > 2 || hdr[1] != 0 || hdr[2] != 0 || hdr[3] != 0 {
			break
		}
		size := int(binary.BigEndian.Uint32(hdr[4:8]))
		if size < 0 || pos+8+size > len(data) {
			break
		}
		stream := "stdout"
		if hdr[0] == 2 {
			stream = "stderr"
		}
		out = append(out, splitLines(string(data[pos+8:pos+8+size]), stream)...)
		pos += 8 + size
	}
	return out, pos > 0
}

func splitLines(payload, stream string) []Line {
	payload = strings.TrimSuffix(payload, "\n")
	if payload == "" {
		return nil
	}
	raw := strings.Split(payload, "\n")
	out := make([]Line, 0, len(raw))
	for _, r := range raw {
		r = strings.TrimRight(r, "\r")
		if r == "" {
			continue
		}
		at, text := splitTimestamp(r)
		out = append(out, Line{Stream: stream, At: at, Text: text})
	}
	return out
}

// splitTimestamp peels off the RFC3339Nano prefix that timestamps=1 prepends.
// Anything that does not parse is returned unchanged, so a line without a
// prefix survives intact rather than losing its first word.
func splitTimestamp(line string) (time.Time, string) {
	idx := strings.IndexByte(line, ' ')
	if idx <= 0 {
		return time.Time{}, line
	}
	at, err := time.Parse(time.RFC3339Nano, line[:idx])
	if err != nil {
		return time.Time{}, line
	}
	return at.UTC(), line[idx+1:]
}
