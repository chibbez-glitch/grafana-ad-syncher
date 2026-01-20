package grafana

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func New(baseURL, adminUser, adminPassword, adminToken string, insecureTLS bool) *Client {
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

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

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
