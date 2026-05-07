package grafana

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Client struct {
	baseURL       string
	adminUser     string
	adminPassword string
	adminToken    string
	httpClient    *http.Client
	debug         bool
	mu            sync.Mutex
	lastOK        time.Time
}

type User struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Login string `json:"login"`
	Email string `json:"email"`
}

type Team struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type TeamMember struct {
	ID    int64  `json:"userId"`
	Name  string `json:"name"`
	Login string `json:"login"`
	Email string `json:"email"`
	Role  string `json:"role"`
}

type OrgUser struct {
	ID    int64  `json:"userId"`
	Login string `json:"login"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

type Folder struct {
	ID    int64  `json:"id"`
	UID   string `json:"uid"`
	Title string `json:"title"`
}

type FolderPermission struct {
	ID             int64  `json:"id"`
	Permission     int    `json:"permission"`
	PermissionName string `json:"permissionName"`
	TeamID         int64  `json:"teamId"`
	Team           string `json:"team"`
	UserID         int64  `json:"userId"`
	User           string `json:"user"`
	Role           string `json:"role"`
}

func New(baseURL, adminUser, adminPassword, adminToken string, insecureTLS, debug bool) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecureTLS {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		adminUser:     adminUser,
		adminPassword: adminPassword,
		adminToken:    adminToken,
		httpClient:    &http.Client{Timeout: 30 * time.Second, Transport: transport},
		debug:         debug,
	}
}

func (c *Client) BaseURL() string { return c.baseURL }

// Debug returns whether verbose connection logging is enabled.
func (c *Client) Debug() bool { return c.debug }

// ProbeResult captures the outcome of a one-shot reachability check.
type ProbeResult struct {
	URL          string
	Host         string
	Port         string
	ResolvedIPs  []string
	ResolveErr   string
	ResolveTook  time.Duration
	TCPAddr      string
	TCPErr       string
	TCPTook      time.Duration
	TLSErr       string
	TLSTook      time.Duration
	TLSVersion   string
	TLSCipher    string
	HealthStatus int
	HealthErr    string
	HealthBody   string
	HealthTook   time.Duration
}

// Probe performs DNS resolution, TCP connect, TLS handshake (if HTTPS) and a
// GET /api/health on the configured base URL. It is meant for one-shot
// diagnostics from startup or from an admin endpoint.
func (c *Client) Probe(ctx context.Context) ProbeResult {
	res := ProbeResult{URL: c.baseURL}
	if c.baseURL == "" {
		res.ResolveErr = "grafana base URL is empty"
		return res
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		res.ResolveErr = "parse url: " + err.Error()
		return res
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	res.Host = host
	res.Port = port

	resolveStart := time.Now()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	res.ResolveTook = time.Since(resolveStart)
	if err != nil {
		res.ResolveErr = err.Error()
		return res
	}
	for _, ip := range ips {
		res.ResolvedIPs = append(res.ResolvedIPs, ip.String())
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tcpStart := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	res.TCPTook = time.Since(tcpStart)
	if err != nil {
		res.TCPErr = err.Error()
		return res
	}
	res.TCPAddr = conn.RemoteAddr().String()

	if u.Scheme == "https" {
		tlsCfg := &tls.Config{ServerName: host}
		if t, ok := c.httpClient.Transport.(*http.Transport); ok && t.TLSClientConfig != nil {
			tlsCfg.InsecureSkipVerify = t.TLSClientConfig.InsecureSkipVerify
		}
		tlsConn := tls.Client(conn, tlsCfg)
		hsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		tlsStart := time.Now()
		err = tlsConn.HandshakeContext(hsCtx)
		res.TLSTook = time.Since(tlsStart)
		if err != nil {
			res.TLSErr = err.Error()
			_ = conn.Close()
			return res
		}
		state := tlsConn.ConnectionState()
		res.TLSVersion = tlsVersionName(state.Version)
		res.TLSCipher = tls.CipherSuiteName(state.CipherSuite)
		_ = tlsConn.Close()
	} else {
		_ = conn.Close()
	}

	healthURL := c.baseURL + "/api/health"
	req, err := http.NewRequestWithContext(ctx, "GET", healthURL, nil)
	if err != nil {
		res.HealthErr = "build request: " + err.Error()
		return res
	}
	req.Header.Set("Accept", "application/json")
	healthStart := time.Now()
	resp, err := c.httpClient.Do(req)
	res.HealthTook = time.Since(healthStart)
	if err != nil {
		res.HealthErr = err.Error()
		return res
	}
	defer resp.Body.Close()
	res.HealthStatus = resp.StatusCode
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	res.HealthBody = strings.TrimSpace(string(body))
	return res
}

// LogProbe writes a multi-line summary of a ProbeResult to the standard logger.
func (c *Client) LogProbe(res ProbeResult) {
	log.Printf("grafana probe: url=%s host=%s port=%s", res.URL, res.Host, res.Port)
	if res.ResolveErr != "" {
		log.Printf("grafana probe: dns FAILED took=%s err=%s", res.ResolveTook, res.ResolveErr)
		return
	}
	log.Printf("grafana probe: dns ok took=%s ips=%s", res.ResolveTook, strings.Join(res.ResolvedIPs, ","))
	if res.TCPErr != "" {
		log.Printf("grafana probe: tcp FAILED took=%s err=%s", res.TCPTook, res.TCPErr)
		return
	}
	log.Printf("grafana probe: tcp ok took=%s peer=%s", res.TCPTook, res.TCPAddr)
	if res.TLSErr != "" {
		log.Printf("grafana probe: tls FAILED took=%s err=%s", res.TLSTook, res.TLSErr)
		return
	}
	if res.TLSTook > 0 {
		log.Printf("grafana probe: tls ok took=%s version=%s cipher=%s", res.TLSTook, res.TLSVersion, res.TLSCipher)
	}
	if res.HealthErr != "" {
		log.Printf("grafana probe: GET /api/health FAILED took=%s err=%s", res.HealthTook, res.HealthErr)
		return
	}
	log.Printf("grafana probe: GET /api/health ok took=%s status=%d body=%q", res.HealthTook, res.HealthStatus, res.HealthBody)
}

func tlsVersionName(v uint16) string {
	switch v {
	case tls.VersionTLS10:
		return "TLS1.0"
	case tls.VersionTLS11:
		return "TLS1.1"
	case tls.VersionTLS12:
		return "TLS1.2"
	case tls.VersionTLS13:
		return "TLS1.3"
	default:
		return fmt.Sprintf("0x%04x", v)
	}
}

func (c *Client) LastOK() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastOK
}

func (c *Client) LookupUser(loginOrEmail string) (*User, bool, error) {
	endpoint := c.baseURL + "/api/users/lookup?loginOrEmail=" + url.QueryEscape(loginOrEmail)
	var user User
	status, err := c.doJSON("GET", endpoint, nil, &user)
	if err != nil {
		if status == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &user, true, nil
}

func (c *Client) CreateUser(email, login, name, password string) (*User, error) {
	payload := map[string]string{
		"name":     name,
		"email":    email,
		"login":    login,
		"password": password,
	}
	endpoint := c.baseURL + "/api/admin/users"
	var resp struct {
		ID int64 `json:"id"`
	}
	if _, err := c.doJSON("POST", endpoint, payload, &resp); err != nil {
		return nil, err
	}
	return &User{ID: resp.ID, Name: name, Login: login, Email: email}, nil
}

func (c *Client) AddUserToOrg(orgID int64, loginOrEmail, role string) error {
	payload := map[string]string{
		"loginOrEmail": loginOrEmail,
		"role":         role,
	}
	endpoint := fmt.Sprintf("%s/api/orgs/%d/users", c.baseURL, orgID)
	status, err := c.doJSON("POST", endpoint, payload, nil)
	if err != nil && status != http.StatusConflict {
		return err
	}
	return nil
}

func (c *Client) UpdateUserRole(orgID, userID int64, role string) error {
	payload := map[string]string{"role": role}
	endpoint := fmt.Sprintf("%s/api/orgs/%d/users/%d", c.baseURL, orgID, userID)
	status, err := c.doJSON("PATCH", endpoint, payload, nil)
	if err != nil && status != http.StatusNotFound {
		return err
	}
	return nil
}

func (c *Client) EnsureTeam(orgID int64, name string) (int64, error) {
	if id, found, err := c.SearchTeam(orgID, name); err == nil && found {
		return id, nil
	}

	createEndpoint := c.baseURL + "/api/teams"
	payload := map[string]any{
		"name":  name,
		"orgId": orgID,
	}
	var createResp struct {
		TeamID int64 `json:"teamId"`
	}
	if _, err := c.doJSON("POST", createEndpoint, payload, &createResp); err != nil {
		return 0, err
	}
	if createResp.TeamID == 0 {
		return 0, fmt.Errorf("grafana: team creation returned empty id")
	}
	return createResp.TeamID, nil
}

func (c *Client) SearchTeam(orgID int64, name string) (int64, bool, error) {
	searchEndpoint := fmt.Sprintf("%s/api/teams/search?name=%s&orgId=%d", c.baseURL, url.QueryEscape(name), orgID)
	var searchResp struct {
		Teams []Team `json:"teams"`
	}
	if _, err := c.doJSON("GET", searchEndpoint, nil, &searchResp); err != nil {
		return 0, false, err
	}
	for _, t := range searchResp.Teams {
		if strings.EqualFold(t.Name, name) {
			return t.ID, true, nil
		}
	}
	return 0, false, nil
}

func (c *Client) ListTeamMembers(teamID int64) ([]TeamMember, error) {
	endpoint := fmt.Sprintf("%s/api/teams/%d/members", c.baseURL, teamID)
	var members []TeamMember
	if _, err := c.doJSON("GET", endpoint, nil, &members); err != nil {
		return nil, err
	}
	return members, nil
}

func (c *Client) ListTeams(orgID int64) ([]Team, error) {
	var teams []Team
	page := 1
	for {
		endpoint := fmt.Sprintf("%s/api/teams/search?orgId=%d&page=%d&perpage=500", c.baseURL, orgID, page)
		var resp struct {
			Teams []Team `json:"teams"`
		}
		if _, err := c.doJSON("GET", endpoint, nil, &resp); err != nil {
			return nil, err
		}
		if len(resp.Teams) == 0 {
			break
		}
		teams = append(teams, resp.Teams...)
		page++
	}
	return teams, nil
}

func (c *Client) ListAdminUsers() ([]User, error) {
	var users []User
	page := 1
	for {
		endpoint := fmt.Sprintf("%s/api/admin/users?page=%d&perpage=1000", c.baseURL, page)
		var resp []User
		if _, err := c.doJSON("GET", endpoint, nil, &resp); err != nil {
			return nil, err
		}
		if len(resp) == 0 {
			break
		}
		users = append(users, resp...)
		page++
	}
	return users, nil
}

func (c *Client) ListOrgUsers(orgID int64) ([]OrgUser, error) {
	endpoint := fmt.Sprintf("%s/api/orgs/%d/users", c.baseURL, orgID)
	var users []OrgUser
	if _, err := c.doJSON("GET", endpoint, nil, &users); err != nil {
		return nil, err
	}
	return users, nil
}

func (c *Client) ListFolders(orgID int64) ([]Folder, error) {
	endpoint := fmt.Sprintf("%s/api/folders", c.baseURL)
	var folders []Folder
	headers := map[string]string{
		"X-Grafana-Org-Id": strconv.FormatInt(orgID, 10),
	}
	if _, err := c.doJSONWithHeaders("GET", endpoint, headers, nil, &folders); err != nil {
		return nil, err
	}
	return folders, nil
}

func (c *Client) ListFolderPermissions(orgID int64, folderUID string) ([]FolderPermission, error) {
	endpoint := fmt.Sprintf("%s/api/folders/%s/permissions", c.baseURL, url.PathEscape(folderUID))
	var perms []FolderPermission
	headers := map[string]string{
		"X-Grafana-Org-Id": strconv.FormatInt(orgID, 10),
	}
	if _, err := c.doJSONWithHeaders("GET", endpoint, headers, nil, &perms); err != nil {
		return nil, err
	}
	return perms, nil
}

func (c *Client) AddUserToTeam(teamID, userID int64, role string) error {
	endpoint := fmt.Sprintf("%s/api/teams/%d/members", c.baseURL, teamID)
	payload := map[string]any{"userId": userID}
	if strings.EqualFold(role, "admin") {
		payload["role"] = "Admin"
	}
	status, err := c.doJSON("POST", endpoint, payload, nil)
	if err != nil && status != http.StatusConflict {
		return err
	}
	return nil
}

func (c *Client) UpdateTeamMemberRole(teamID, userID int64, role string) error {
	endpoint := fmt.Sprintf("%s/api/teams/%d/members/%d", c.baseURL, teamID, userID)
	payload := map[string]string{"role": "Member"}
	if strings.EqualFold(role, "admin") {
		payload["role"] = "Admin"
	}
	status, err := c.doJSON("PUT", endpoint, payload, nil)
	if err != nil && status != http.StatusNotFound {
		return err
	}
	return nil
}

func (c *Client) RemoveUserFromTeam(teamID, userID int64) error {
	endpoint := fmt.Sprintf("%s/api/teams/%d/members/%d", c.baseURL, teamID, userID)
	status, err := c.doJSON("DELETE", endpoint, nil, nil)
	if err != nil && status != http.StatusNotFound {
		return err
	}
	return nil
}

func (c *Client) doJSON(method, endpoint string, body any, out any) (int, error) {
	return c.doJSONWithHeaders(method, endpoint, nil, body, out)
}

func (c *Client) doJSONWithHeaders(method, endpoint string, headers map[string]string, body any, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		buf := &bytes.Buffer{}
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			return 0, err
		}
		reader = buf
	}
	req, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.adminToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.adminToken)
	} else if c.adminUser != "" || c.adminPassword != "" {
		req.SetBasicAuth(c.adminUser, c.adminPassword)
	}
	for key, value := range headers {
		if key == "" {
			continue
		}
		req.Header.Set(key, value)
	}

	var trace *requestTrace
	if c.debug {
		trace = newRequestTrace()
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), trace.clientTrace()))
	}

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		if c.debug {
			log.Printf("grafana http: %s %s FAILED took=%s err=%v %s", method, endpoint, elapsed.Round(time.Millisecond), err, trace.summary())
		}
		return 0, err
	}
	defer resp.Body.Close()

	if c.debug {
		log.Printf("grafana http: %s %s status=%d took=%s %s", method, endpoint, resp.StatusCode, elapsed.Round(time.Millisecond), trace.summary())
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, fmt.Errorf("grafana: %s %s -> %d: %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	c.mu.Lock()
	c.lastOK = time.Now().UTC()
	c.mu.Unlock()

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// requestTrace collects timings and resolved addresses for a single HTTP
// request via net/http/httptrace.
type requestTrace struct {
	mu sync.Mutex

	dnsStart     time.Time
	dnsDone      time.Time
	dnsHost      string
	dnsAddrs     []string
	dnsErr       string

	connectStart time.Time
	connectDone  time.Time
	connectAddr  string
	connectErr   string

	tlsStart time.Time
	tlsDone  time.Time
	tlsErr   string
	tlsVer   string

	gotConnAt time.Time
	reused    bool
	wasIdle   bool
	idleTime  time.Duration
	localAddr string
	remoteAddr string

	wroteRequest time.Time
	gotFirstByte time.Time

	startedAt time.Time
}

func newRequestTrace() *requestTrace {
	return &requestTrace{startedAt: time.Now()}
}

func (t *requestTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.dnsStart = time.Now()
			t.dnsHost = info.Host
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.dnsDone = time.Now()
			for _, a := range info.Addrs {
				t.dnsAddrs = append(t.dnsAddrs, a.IP.String())
			}
			if info.Err != nil {
				t.dnsErr = info.Err.Error()
			}
		},
		ConnectStart: func(network, addr string) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.connectStart = time.Now()
			t.connectAddr = addr
		},
		ConnectDone: func(network, addr string, err error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.connectDone = time.Now()
			if err != nil {
				t.connectErr = err.Error()
			}
		},
		TLSHandshakeStart: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.tlsStart = time.Now()
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.tlsDone = time.Now()
			if err != nil {
				t.tlsErr = err.Error()
				return
			}
			t.tlsVer = tlsVersionName(state.Version)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.gotConnAt = time.Now()
			t.reused = info.Reused
			t.wasIdle = info.WasIdle
			t.idleTime = info.IdleTime
			if info.Conn != nil {
				if la := info.Conn.LocalAddr(); la != nil {
					t.localAddr = la.String()
				}
				if ra := info.Conn.RemoteAddr(); ra != nil {
					t.remoteAddr = ra.String()
				}
			}
		},
		WroteRequest: func(_ httptrace.WroteRequestInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.wroteRequest = time.Now()
		},
		GotFirstResponseByte: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			t.gotFirstByte = time.Now()
		},
	}
}

// summary builds a single-line trace summary safe for logs.
func (t *requestTrace) summary() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	parts := []string{}
	if !t.dnsStart.IsZero() {
		dur := durOrZero(t.dnsStart, t.dnsDone)
		ips := strings.Join(t.dnsAddrs, ",")
		if t.dnsErr != "" {
			parts = append(parts, fmt.Sprintf("dns=%s ips=%s dnsErr=%q", dur, ips, t.dnsErr))
		} else {
			parts = append(parts, fmt.Sprintf("dns=%s ips=%s", dur, ips))
		}
	}
	if !t.connectStart.IsZero() {
		dur := durOrZero(t.connectStart, t.connectDone)
		if t.connectErr != "" {
			parts = append(parts, fmt.Sprintf("connect=%s addr=%s connectErr=%q", dur, t.connectAddr, t.connectErr))
		} else {
			parts = append(parts, fmt.Sprintf("connect=%s addr=%s", dur, t.connectAddr))
		}
	}
	if !t.tlsStart.IsZero() {
		dur := durOrZero(t.tlsStart, t.tlsDone)
		if t.tlsErr != "" {
			parts = append(parts, fmt.Sprintf("tls=%s tlsErr=%q", dur, t.tlsErr))
		} else {
			parts = append(parts, fmt.Sprintf("tls=%s ver=%s", dur, t.tlsVer))
		}
	}
	if !t.gotConnAt.IsZero() {
		parts = append(parts, fmt.Sprintf("conn=reused=%t idle=%s remote=%s local=%s", t.reused, t.idleTime.Round(time.Millisecond), t.remoteAddr, t.localAddr))
	}
	if !t.gotFirstByte.IsZero() {
		parts = append(parts, fmt.Sprintf("ttfb=%s", t.gotFirstByte.Sub(t.startedAt).Round(time.Millisecond)))
	} else if !t.wroteRequest.IsZero() {
		parts = append(parts, fmt.Sprintf("wrote=%s (no first byte)", t.wroteRequest.Sub(t.startedAt).Round(time.Millisecond)))
	}
	return strings.Join(parts, " ")
}

func durOrZero(start, end time.Time) time.Duration {
	if start.IsZero() || end.IsZero() {
		return 0
	}
	return end.Sub(start).Round(time.Millisecond)
}
