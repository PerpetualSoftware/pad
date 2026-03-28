package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/xarmian/pad/internal/models"
)

// Client is a thin HTTP client for the Pad API.
type Client struct {
	baseURL    string
	httpClient *http.Client
	authToken  string // session or API token, sent as Authorization: Bearer
	agentName  string // optional agent name, sent as X-Pad-Agent header
}

func NewClient(host string, port int) *Client {
	return NewClientFromURL(fmt.Sprintf("http://%s:%d", host, port))
}

// NewClientFromURL creates a client from a full base URL (e.g., "https://api.getpad.dev").
func NewClientFromURL(baseURL string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	c := &Client{
		baseURL: baseURL + "/api/v1",
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}

	// Auto-load credentials if available
	if creds, err := LoadCredentials(); err == nil && creds != nil {
		c.authToken = creds.Token
	}

	// Auto-load agent name from .pad.toml if available
	if pt, _ := LoadPadToml(); pt != nil && pt.AgentName != "" {
		c.agentName = pt.AgentName
	}

	return c
}

// SetAuthToken sets the authorization token for API requests.
func (c *Client) SetAuthToken(token string) {
	c.authToken = token
}

// Health checks if the server is running.
func (c *Client) Health() error {
	req, err := c.newRequest("GET", "/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: status %d", resp.StatusCode)
	}
	return nil
}

// --- Workspaces ---

func (c *Client) ListWorkspaces() ([]models.Workspace, error) {
	var result []models.Workspace
	return result, c.get("/workspaces", &result)
}

func (c *Client) CreateWorkspace(input models.WorkspaceCreate) (*models.Workspace, error) {
	var result models.Workspace
	return &result, c.post("/workspaces", input, &result)
}

func (c *Client) GetWorkspace(slug string) (*models.Workspace, error) {
	var result models.Workspace
	return &result, c.get("/workspaces/"+slug, &result)
}

func (c *Client) UpdateWorkspace(slug string, input models.WorkspaceUpdate) (*models.Workspace, error) {
	var result models.Workspace
	return &result, c.patch("/workspaces/"+slug, input, &result)
}

// --- Collections ---

func (c *Client) ListCollections(wsSlug string) ([]models.Collection, error) {
	var result []models.Collection
	return result, c.get("/workspaces/"+wsSlug+"/collections", &result)
}

func (c *Client) CreateCollection(wsSlug string, input models.CollectionCreate) (*models.Collection, error) {
	var result models.Collection
	return &result, c.post("/workspaces/"+wsSlug+"/collections", input, &result)
}

func (c *Client) GetCollection(wsSlug, collSlug string) (*models.Collection, error) {
	var result models.Collection
	return &result, c.get("/workspaces/"+wsSlug+"/collections/"+collSlug, &result)
}

func (c *Client) UpdateCollection(wsSlug, collSlug string, input models.CollectionUpdate) (*models.Collection, error) {
	var result models.Collection
	return &result, c.patch("/workspaces/"+wsSlug+"/collections/"+collSlug, input, &result)
}

// --- Items ---

// ListItems returns items across all collections in a workspace.
// Use params for filtering, sorting, grouping, pagination, etc.
func (c *Client) ListItems(wsSlug string, params url.Values) ([]models.Item, error) {
	var result []models.Item
	path := "/workspaces/" + wsSlug + "/items"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	return result, c.get(path, &result)
}

// ListCollectionItems returns items within a specific collection.
func (c *Client) ListCollectionItems(wsSlug, collSlug string, params url.Values) ([]models.Item, error) {
	var result []models.Item
	path := "/workspaces/" + wsSlug + "/collections/" + collSlug + "/items"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	return result, c.get(path, &result)
}

func (c *Client) CreateItem(wsSlug, collSlug string, input models.ItemCreate) (*models.Item, error) {
	var result models.Item
	return &result, c.post("/workspaces/"+wsSlug+"/collections/"+collSlug+"/items", input, &result)
}

func (c *Client) GetItem(wsSlug, itemSlug string) (*models.Item, error) {
	var result models.Item
	return &result, c.get("/workspaces/"+wsSlug+"/items/"+itemSlug, &result)
}

func (c *Client) UpdateItem(wsSlug, itemSlug string, input models.ItemUpdate) (*models.Item, error) {
	var result models.Item
	return &result, c.patch("/workspaces/"+wsSlug+"/items/"+itemSlug, input, &result)
}

func (c *Client) DeleteItem(wsSlug, itemSlug string) error {
	return c.delete("/workspaces/" + wsSlug + "/items/" + itemSlug)
}

func (c *Client) MoveItem(wsSlug, itemSlug string, input map[string]any) (*models.Item, error) {
	var result models.Item
	return &result, c.post("/workspaces/"+wsSlug+"/items/"+itemSlug+"/move", input, &result)
}

// --- Links ---

func (c *Client) GetItemLinks(wsSlug, itemSlug string) ([]models.ItemLink, error) {
	var result []models.ItemLink
	return result, c.get("/workspaces/"+wsSlug+"/items/"+itemSlug+"/links", &result)
}

func (c *Client) CreateItemLink(wsSlug, itemSlug string, input models.ItemLinkCreate) (*models.ItemLink, error) {
	var result models.ItemLink
	return &result, c.post("/workspaces/"+wsSlug+"/items/"+itemSlug+"/links", input, &result)
}

func (c *Client) DeleteItemLink(wsSlug, linkID string) error {
	return c.delete("/workspaces/" + wsSlug + "/links/" + linkID)
}

// --- Comments ---

func (c *Client) ListComments(wsSlug, itemSlug string) ([]models.Comment, error) {
	var result []models.Comment
	return result, c.get("/workspaces/"+wsSlug+"/items/"+itemSlug+"/comments", &result)
}

func (c *Client) CreateComment(wsSlug, itemSlug string, input models.CommentCreate) (*models.Comment, error) {
	var result models.Comment
	return &result, c.post("/workspaces/"+wsSlug+"/items/"+itemSlug+"/comments", input, &result)
}

func (c *Client) DeleteComment(wsSlug, commentID string) error {
	return c.delete("/workspaces/" + wsSlug + "/comments/" + commentID)
}

// --- Dashboard ---

// GetDashboard returns the workspace dashboard as raw JSON.
// The DashboardResponse type lives in the server package, so we use json.RawMessage.
func (c *Client) GetDashboard(wsSlug string) (json.RawMessage, error) {
	var result json.RawMessage
	return result, c.get("/workspaces/"+wsSlug+"/dashboard", &result)
}

// --- Search ---

// SearchItems performs a cross-workspace search. Pass q, workspace, etc. via params.
func (c *Client) SearchItems(params url.Values) (json.RawMessage, error) {
	var result json.RawMessage
	path := "/search"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	return result, c.get(path, &result)
}

// --- Activity ---

func (c *Client) ListActivity(wsSlug string, params url.Values) ([]models.Activity, error) {
	var result []models.Activity
	path := "/workspaces/" + wsSlug + "/activity"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}
	return result, c.get(path, &result)
}

// --- Convention Library ---

// ConventionLibraryResponse is the response from the convention-library endpoint.
type ConventionLibraryResponse struct {
	Categories []LibraryCategory `json:"categories"`
}

// LibraryCategory groups related conventions under a named category.
type LibraryCategory struct {
	Name        string              `json:"name"`
	Description string              `json:"description"`
	Conventions []LibraryConvention `json:"conventions"`
}

// LibraryConvention holds a pre-built convention definition.
type LibraryConvention struct {
	Title    string `json:"title"`
	Content  string `json:"content"`
	Category string `json:"category"`
	Trigger  string `json:"trigger"`
	Scope    string `json:"scope"`
	Priority string `json:"priority"`
}

// GetConventionLibrary fetches the convention library from the server.
func (c *Client) GetConventionLibrary() (*ConventionLibraryResponse, error) {
	var result ConventionLibraryResponse
	return &result, c.get("/convention-library", &result)
}

// --- Playbook Library ---

// PlaybookLibraryResponse is the response from the playbook-library endpoint.
type PlaybookLibraryResponse struct {
	Categories []PlaybookCategory `json:"categories"`
}

// PlaybookCategory groups related playbooks under a named category.
type PlaybookCategory struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Playbooks   []LibraryPlaybook `json:"playbooks"`
}

// LibraryPlaybook holds a pre-built playbook definition.
type LibraryPlaybook struct {
	Title    string `json:"title"`
	Content  string `json:"content"`
	Category string `json:"category"`
	Trigger  string `json:"trigger"`
	Scope    string `json:"scope"`
}

// GetPlaybookLibrary fetches the playbook library from the server.
func (c *Client) GetPlaybookLibrary() (*PlaybookLibraryResponse, error) {
	var result PlaybookLibraryResponse
	return &result, c.get("/playbook-library", &result)
}

// --- Webhooks ---

// ListWebhooks returns all webhooks for a workspace.
func (c *Client) ListWebhooks(wsSlug string) ([]models.Webhook, error) {
	var result []models.Webhook
	return result, c.get("/workspaces/"+wsSlug+"/webhooks", &result)
}

// CreateWebhook registers a new webhook for a workspace.
func (c *Client) CreateWebhook(wsSlug string, input models.WebhookCreate) (*models.Webhook, error) {
	var result models.Webhook
	return &result, c.post("/workspaces/"+wsSlug+"/webhooks", input, &result)
}

// DeleteWebhook removes a webhook by ID.
func (c *Client) DeleteWebhook(wsSlug, webhookID string) error {
	return c.delete("/workspaces/" + wsSlug + "/webhooks/" + webhookID)
}

// TestWebhook sends a test payload to a webhook.
func (c *Client) TestWebhook(wsSlug, webhookID string) error {
	return c.post("/workspaces/"+wsSlug+"/webhooks/"+webhookID+"/test", nil, nil)
}

// --- Export / Import ---

// RawGet fetches raw bytes from the API.
func (c *Client) RawGet(path string) ([]byte, error) {
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, c.parseError(resp)
	}
	return io.ReadAll(resp.Body)
}

// PostRaw sends raw bytes to the API and decodes the JSON response.
func (c *Client) PostRaw(path string, data []byte, result interface{}) error {
	req, err := c.newRequest("POST", path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	return c.handleResponse(resp, result)
}

// --- Auth API ---

// LoginResponse is the response from POST /auth/login.
type LoginResponse struct {
	User  LoginUser `json:"user"`
	Token string    `json:"token"`
}

// LoginUser is the user info returned from auth endpoints.
type LoginUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

// SessionResponse is the response from GET /auth/session.
type SessionResponse struct {
	Authenticated bool      `json:"authenticated"`
	NeedsSetup    bool      `json:"needs_setup"`
	User          LoginUser `json:"user"`
}

// Login authenticates with email and password.
func (c *Client) Login(email, password string) (*LoginResponse, error) {
	var result LoginResponse
	err := c.post("/auth/login", map[string]string{
		"email":    email,
		"password": password,
	}, &result)
	return &result, err
}

// Register creates a new user account.
func (c *Client) Register(email, name, password string) (*LoginResponse, error) {
	var result LoginResponse
	err := c.post("/auth/register", map[string]string{
		"email":    email,
		"name":     name,
		"password": password,
	}, &result)
	return &result, err
}

// Logout destroys the current session.
func (c *Client) Logout() error {
	return c.post("/auth/logout", nil, nil)
}

// GetCurrentUser returns the authenticated user's profile.
func (c *Client) GetCurrentUser() (*LoginUser, error) {
	var result LoginUser
	return &result, c.get("/auth/me", &result)
}

// CheckSession returns the current auth status.
func (c *Client) CheckSession() (*SessionResponse, error) {
	var result SessionResponse
	return &result, c.get("/auth/session", &result)
}

// --- HTTP helpers ---

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return e.Message
}

// newRequest creates an http.Request with auth and agent headers set.
func (c *Client) newRequest(method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	if c.agentName != "" {
		req.Header.Set("X-Pad-Agent", c.agentName)
	}
	return req, nil
}

func (c *Client) get(path string, result interface{}) error {
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	return c.handleResponse(resp, result)
}

func (c *Client) post(path string, body interface{}, result interface{}) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}
	req, err := c.newRequest("POST", path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	return c.handleResponse(resp, result)
}

func (c *Client) patch(path string, body interface{}, result interface{}) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := c.newRequest("PATCH", path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	return c.handleResponse(resp, result)
}

func (c *Client) delete(path string) error {
	req, err := c.newRequest("DELETE", path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode >= 400 {
		return c.parseError(resp)
	}
	return nil
}

func (c *Client) handleResponse(resp *http.Response, result interface{}) error {
	if resp.StatusCode >= 400 {
		return c.parseError(resp)
	}
	if result != nil {
		return json.NewDecoder(resp.Body).Decode(result)
	}
	return nil
}

func (c *Client) parseError(resp *http.Response) error {
	var errResp struct {
		Error APIError `json:"error"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error.Message != "" {
		return &errResp.Error
	}
	return fmt.Errorf("API error: %d %s", resp.StatusCode, string(body))
}
