package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
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

// StarItem stars an item for the current user.
func (c *Client) StarItem(wsSlug, itemSlug string) error {
	return c.post("/workspaces/"+wsSlug+"/items/"+itemSlug+"/star", nil, nil)
}

// UnstarItem removes a star from an item for the current user.
func (c *Client) UnstarItem(wsSlug, itemSlug string) error {
	return c.delete("/workspaces/" + wsSlug + "/items/" + itemSlug + "/star")
}

// ListStarredItems returns the current user's starred items in a workspace.
func (c *Client) ListStarredItems(wsSlug string, includeTerminal bool) ([]models.Item, error) {
	var result []models.Item
	path := "/workspaces/" + wsSlug + "/starred"
	if includeTerminal {
		path += "?include_terminal=true"
	}
	return result, c.get(path, &result)
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
	Title       string   `json:"title"`
	Content     string   `json:"content"`
	Category    string   `json:"category"`
	Trigger     string   `json:"trigger"`
	Surfaces    []string `json:"surfaces"`
	Enforcement string   `json:"enforcement"`
	Commands    []string `json:"commands"`
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

// --- Workspace Members ---

// ListWorkspaceMembers returns all members of a workspace.
func (c *Client) ListWorkspaceMembers(wsSlug string) ([]models.WorkspaceMember, error) {
	var result struct {
		Members []models.WorkspaceMember `json:"members"`
	}
	if err := c.get("/workspaces/"+wsSlug+"/members", &result); err != nil {
		return nil, err
	}
	return result.Members, nil
}

// --- Agent Roles ---

// ListAgentRoles returns all agent roles for a workspace.
func (c *Client) ListAgentRoles(wsSlug string) ([]models.AgentRole, error) {
	var result []models.AgentRole
	return result, c.get("/workspaces/"+wsSlug+"/agent-roles", &result)
}

// CreateAgentRole creates a new agent role in a workspace.
func (c *Client) CreateAgentRole(wsSlug string, input models.AgentRoleCreate) (*models.AgentRole, error) {
	var result models.AgentRole
	return &result, c.post("/workspaces/"+wsSlug+"/agent-roles", input, &result)
}

// GetAgentRole gets a single agent role by ID or slug.
func (c *Client) GetAgentRole(wsSlug, idOrSlug string) (*models.AgentRole, error) {
	var result models.AgentRole
	return &result, c.get("/workspaces/"+wsSlug+"/agent-roles/"+idOrSlug, &result)
}

// UpdateAgentRole updates an existing agent role.
func (c *Client) UpdateAgentRole(wsSlug, idOrSlug string, input models.AgentRoleUpdate) (*models.AgentRole, error) {
	var result models.AgentRole
	return &result, c.patch("/workspaces/"+wsSlug+"/agent-roles/"+idOrSlug, input, &result)
}

// DeleteAgentRole removes an agent role from a workspace.
func (c *Client) DeleteAgentRole(wsSlug, idOrSlug string) error {
	return c.delete("/workspaces/" + wsSlug + "/agent-roles/" + idOrSlug)
}

// --- Export / Import ---

// RawGet fetches raw bytes from the API.
//
// Buffers the entire response in memory; do NOT use for endpoints that
// can return arbitrarily large bodies (e.g. workspace export bundles).
// Reach for RawStream for those callers — see TASK-884 review feedback.
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

// RawStream issues a GET and copies the response body into w as it
// arrives. Returns the number of bytes written and a *http.Response
// pointer the caller can inspect for trailers (used by the export
// bundle path to verify X-Bundle-Status). Used for large payloads
// (workspace export bundles, future S3-backed downloads) where
// buffering the whole body would defeat the server's streaming
// design and risk OOM on multi-GB exports.
//
// The HTTP status check still consumes the body if non-2xx (so the
// error message can include the server's response), but only for the
// error case — the happy path streams directly.
//
// IMPORTANT: trailers are only populated AFTER the body has been
// fully consumed (Go runtime guarantee). Callers that want to read
// resp.Trailer must wait until after io.Copy returns.
func (c *Client) RawStream(path string, w io.Writer) (int64, *http.Response, error) {
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, resp, c.parseError(resp)
	}
	n, err := io.Copy(w, resp.Body)
	return n, resp, err
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
	User           LoginUser `json:"user"`
	Token          string    `json:"token"`
	Requires2FA    bool      `json:"requires_2fa,omitempty"`
	ChallengeToken string    `json:"challenge_token,omitempty"`
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
	SetupRequired bool      `json:"setup_required"`
	SetupMethod   string    `json:"setup_method"`
	AuthMethod    string    `json:"auth_method"`
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

// LoginVerify2FA completes a 2FA login by submitting a TOTP or recovery code.
func (c *Client) LoginVerify2FA(challengeToken, code, recoveryCode string) (*LoginResponse, error) {
	var result LoginResponse
	body := map[string]string{
		"challenge_token": challengeToken,
	}
	if code != "" {
		body["code"] = code
	}
	if recoveryCode != "" {
		body["recovery_code"] = recoveryCode
	}
	err := c.post("/auth/2fa/login-verify", body, &result)
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

// Bootstrap creates the first admin account on a fresh instance.
func (c *Client) Bootstrap(email, name, password string) (*LoginResponse, error) {
	var result LoginResponse
	err := c.post("/auth/bootstrap", map[string]string{
		"email":    email,
		"name":     name,
		"password": password,
	}, &result)
	return &result, err
}

// CLIAuthSessionResponse is the response from POST /auth/cli/sessions.
type CLIAuthSessionResponse struct {
	SessionCode string `json:"session_code"`
	AuthURL     string `json:"auth_url"`
	ExpiresAt   string `json:"expires_at"`
}

// CLIAuthSessionStatus is the response from GET /auth/cli/sessions/{code}.
type CLIAuthSessionStatus struct {
	Status string    `json:"status"` // "pending", "approved", "expired"
	Token  string    `json:"token,omitempty"`
	User   LoginUser `json:"user,omitempty"`
}

// CreateCLIAuthSession creates a new pending CLI auth session.
func (c *Client) CreateCLIAuthSession() (*CLIAuthSessionResponse, error) {
	var result CLIAuthSessionResponse
	err := c.post("/auth/cli/sessions", nil, &result)
	return &result, err
}

// PollCLIAuthSession checks the status of a CLI auth session.
func (c *Client) PollCLIAuthSession(code string) (*CLIAuthSessionStatus, error) {
	var result CLIAuthSessionStatus
	err := c.get("/auth/cli/sessions/"+code, &result)
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

// --- Audit Log ---

// GetAuditLog fetches the global audit log (admin-only).
func (c *Client) GetAuditLog(params models.AuditLogParams) ([]models.Activity, error) {
	q := url.Values{}
	if params.Action != "" {
		q.Set("action", params.Action)
	}
	if params.Actor != "" {
		q.Set("actor", params.Actor)
	}
	if params.WorkspaceID != "" {
		q.Set("workspace", params.WorkspaceID)
	}
	if params.Days > 0 {
		q.Set("days", fmt.Sprintf("%d", params.Days))
	}
	if params.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", params.Limit))
	}
	if params.Offset > 0 {
		q.Set("offset", fmt.Sprintf("%d", params.Offset))
	}
	path := "/audit-log"
	if qs := q.Encode(); qs != "" {
		path += "?" + qs
	}
	var result []models.Activity
	return result, c.get(path, &result)
}

// --- HTTP helpers ---

type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	return e.Message
}

// --- Attachments ---
//
// AttachmentUploadResult mirrors the JSON returned by
// POST /api/v1/workspaces/{slug}/attachments. It is the API contract
// callers depend on; do NOT replace it with models.Attachment which has
// different field names and embeds DB-only fields.
type AttachmentUploadResult struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	MIME       string `json:"mime"`
	Size       int64  `json:"size"`
	Width      *int   `json:"width,omitempty"`
	Height     *int   `json:"height,omitempty"`
	Filename   string `json:"filename"`
	Category   string `json:"category"`
	RenderMode string `json:"render_mode"`
}

// UploadAttachment streams the contents of body to
// POST /api/v1/workspaces/{wsSlug}/attachments as a multipart file
// part. filename is what the server stores (after basenaming); itemRef
// is optional and associates the upload with a parent item via the
// item_id form field — pass empty string for a free-floating upload.
//
// The caller is responsible for closing body if it's a *os.File or
// other io.Closer; this method only reads from it.
func (c *Client) UploadAttachment(wsSlug, itemRef, filename string, body io.Reader) (*AttachmentUploadResult, error) {
	// Build the multipart envelope into a pipe so we don't have to
	// buffer the entire upload in memory before sending.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	// Spawn a goroutine that writes the multipart body. We can't write
	// inline because the http.Request.Body needs to be a Reader the
	// transport pulls from in parallel with us writing.
	go func() {
		defer pw.Close()
		defer mw.Close()

		if itemRef != "" {
			if err := mw.WriteField("item_id", itemRef); err != nil {
				_ = pw.CloseWithError(fmt.Errorf("write item_id field: %w", err))
				return
			}
		}
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			_ = pw.CloseWithError(fmt.Errorf("create file part: %w", err))
			return
		}
		if _, err := io.Copy(part, body); err != nil {
			_ = pw.CloseWithError(fmt.Errorf("stream upload body: %w", err))
			return
		}
	}()

	req, err := c.newRequest("POST", "/workspaces/"+wsSlug+"/attachments", pr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	// Uploads can be large and slow over a remote link. The default
	// 10s ClientTimeout is too tight for a 25 MiB upload over a
	// constrained connection, so use a fresh client with a generous
	// timeout for this single request only.
	uploadClient := &http.Client{Timeout: 5 * time.Minute}
	resp, err := uploadClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload attachment: %w", err)
	}
	defer resp.Body.Close()
	var result AttachmentUploadResult
	if err := c.handleResponse(resp, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// DownloadAttachment streams the bytes of an attachment into w. Returns
// the Content-Type the server set and the number of bytes copied so
// callers can verify size or render with the right MIME hint.
//
// When variant is non-empty, requests ?variant=<variant>; the server
// silently falls back to the original if the derived row doesn't exist
// (TASK-872 / TASK-878 contract).
func (c *Client) DownloadAttachment(wsSlug, attachmentID, variant string, w io.Writer) (mime string, size int64, err error) {
	path := "/workspaces/" + wsSlug + "/attachments/" + attachmentID
	if variant != "" {
		path += "?variant=" + url.QueryEscape(variant)
	}
	req, err := c.newRequest("GET", path, nil)
	if err != nil {
		return "", 0, err
	}
	// Use a generous timeout — large blobs over a slow link otherwise
	// trip the default 10s on the package-shared client.
	dlClient := &http.Client{Timeout: 5 * time.Minute}
	resp, err := dlClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("download attachment: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", 0, c.parseError(resp)
	}
	n, copyErr := io.Copy(w, resp.Body)
	if copyErr != nil {
		return resp.Header.Get("Content-Type"), n, fmt.Errorf("stream download: %w", copyErr)
	}
	return resp.Header.Get("Content-Type"), n, nil
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
