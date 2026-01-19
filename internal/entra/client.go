package entra

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Client struct {
	ten        string
	clientID   string
	secret     string
	authBase   string
	graphBase  string
	httpClient *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

type Member struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Mail        string `json:"mail"`
	UPN         string `json:"userPrincipalName"`
}

type Group struct {
	ID              string `json:"id"`
	DisplayName     string `json:"displayName"`
	Mail            string `json:"mail"`
	SecurityEnabled bool   `json:"securityEnabled"`
	MailEnabled     bool   `json:"mailEnabled"`
}

type User struct {
	ID             string `json:"id"`
	DisplayName    string `json:"displayName"`
	Mail           string `json:"mail"`
	UPN            string `json:"userPrincipalName"`
	AccountEnabled bool   `json:"accountEnabled"`
}

func New(tenantID, clientID, clientSecret, authBase, graphBase string) *Client {
	return &Client{
		ten:        tenantID,
		clientID:   clientID,
		secret:     clientSecret,
		authBase:   strings.TrimRight(authBase, "/"),
		graphBase:  strings.TrimRight(graphBase, "/"),
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *Client) ListGroupMembers(groupID string) ([]Member, error) {
	token, err := c.getToken()
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/groups/%s/members?$select=id,displayName,mail,userPrincipalName", c.graphBase, url.PathEscape(groupID))
	var members []Member
	for endpoint != "" {
		resp, err := c.doRequest("GET", endpoint, token, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Value    []Member `json:"value"`
			NextLink string   `json:"@odata.nextLink"`
		}
		if err := json.NewDecoder(resp).Decode(&page); err != nil {
			_ = resp.Close()
			return nil, err
		}
		_ = resp.Close()
		members = append(members, page.Value...)
		endpoint = page.NextLink
	}
	return members, nil
}

func (c *Client) ListGroups() ([]Group, error) {
	token, err := c.getToken()
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/groups?$select=id,displayName,mail,securityEnabled,mailEnabled", c.graphBase)
	var groups []Group
	for endpoint != "" {
		resp, err := c.doRequest("GET", endpoint, token, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Value    []Group `json:"value"`
			NextLink string  `json:"@odata.nextLink"`
		}
		if err := json.NewDecoder(resp).Decode(&page); err != nil {
			_ = resp.Close()
			return nil, err
		}
		_ = resp.Close()
		groups = append(groups, page.Value...)
		endpoint = page.NextLink
	}
	return groups, nil
}

func (c *Client) ListUsers() ([]User, error) {
	token, err := c.getToken()
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("%s/users?$select=id,displayName,mail,userPrincipalName,accountEnabled", c.graphBase)
	var users []User
	for endpoint != "" {
		resp, err := c.doRequest("GET", endpoint, token, nil)
		if err != nil {
			return nil, err
		}
		var page struct {
			Value    []User `json:"value"`
			NextLink string `json:"@odata.nextLink"`
		}
		if err := json.NewDecoder(resp).Decode(&page); err != nil {
			_ = resp.Close()
			return nil, err
		}
		_ = resp.Close()
		users = append(users, page.Value...)
		endpoint = page.NextLink
	}
	return users, nil
}

func (c *Client) getToken() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.accessToken != "" && time.Until(c.expiresAt) > 2*time.Minute {
		return c.accessToken, nil
	}

	endpoint := fmt.Sprintf("%s/%s/oauth2/v2.0/token", c.authBase, c.ten)
	form := url.Values{}
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.secret)
	form.Set("scope", "https://graph.microsoft.com/.default")
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequest("POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("entra: token request failed: %s", strings.TrimSpace(string(payload)))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}
	if tokenResp.AccessToken == "" {
		return "", errors.New("entra: empty access token")
	}

	c.accessToken = tokenResp.AccessToken
	c.expiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	return c.accessToken, nil
}

func (c *Client) doRequest(method, endpoint, token string, body any) (io.ReadCloser, error) {
	var reader io.Reader
	if body != nil {
		buf := &bytes.Buffer{}
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			return nil, err
		}
		reader = buf
	}
	if strings.HasPrefix(endpoint, "https://") == false && strings.HasPrefix(endpoint, "http://") == false {
		endpoint = c.graphBase + "/" + strings.TrimLeft(endpoint, "/")
	}
	req, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("entra: %s %s -> %d: %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	return resp.Body, nil
}
