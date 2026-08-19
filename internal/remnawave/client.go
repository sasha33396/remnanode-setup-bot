package remnawave

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	nodePort        = 2222
	maxResponseSize = 4 << 20
)

var (
	// ErrUnauthorized indicates a 401 or 403 response. It never contains the
	// bearer token or Authorization header.
	ErrUnauthorized = errors.New("Remnawave authorization failed")
	// ErrInvalidResponse indicates malformed or incomplete API JSON.
	ErrInvalidResponse = errors.New("invalid Remnawave API response")
)

// API is the deployment-facing subset of the supplied Remnawave OpenAPI.
type API interface {
	GetHosts(context.Context) ([]Host, error)
	GenerateSecretKey(context.Context) (string, error)
	GetNodes(context.Context) ([]Node, error)
	GetNode(context.Context, string) (Node, error)
	GetNodesMetrics(context.Context) ([]NodeMetric, error)
	CreateNode(context.Context, CreateNodeInput) (Node, error)
	UpdateNodeAddress(context.Context, UpdateNodeAddressInput) (Node, error)
	UpdateNodeProfile(context.Context, UpdateNodeProfileInput) (Node, error)
	DeleteNode(context.Context, string) (bool, error)
}

// GetNodesMetrics calls the official system metrics endpoint. usersOnline is
// joined to /api/nodes by UUID by higher layers; names are presentation-only.
func (c *Client) GetNodesMetrics(ctx context.Context) ([]NodeMetric, error) {
	var envelope nodeMetricsEnvelope
	if err := c.doJSON(ctx, http.MethodGet, "/api/system/nodes/metrics", nil, http.StatusOK, &envelope); err != nil {
		return nil, err
	}
	if envelope.Response == nil || envelope.Response.Nodes == nil {
		return nil, invalidResponse("node metrics response is missing")
	}
	result := make([]NodeMetric, 0, len(*envelope.Response.Nodes))
	for index, item := range *envelope.Response.Nodes {
		metric, err := item.model()
		if err != nil {
			return nil, invalidResponse(fmt.Sprintf("node metric %d is incomplete", index))
		}
		result = append(result, metric)
	}
	return result, nil
}

// Client is a Remnawave HTTP API client.
type Client struct {
	baseURL *url.URL
	token   string
	http    *http.Client
}

var _ API = (*Client)(nil)

// NewClient creates a client with a mandatory explicit timeout.
func NewClient(baseURL, token string, timeout time.Duration) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("base URL: %w", ErrInvalidInput)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("bearer token: %w", ErrInvalidInput)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("HTTP timeout: %w", ErrInvalidInput)
	}
	return &Client{
		baseURL: parsed,
		token:   token,
		http:    &http.Client{Timeout: timeout},
	}, nil
}

// GetHosts calls HostsController_getAllHosts (GET /api/hosts).
func (c *Client) GetHosts(ctx context.Context) ([]Host, error) {
	var envelope hostsEnvelope
	if err := c.doJSON(ctx, http.MethodGet, "/api/hosts", nil, http.StatusOK, &envelope); err != nil {
		return nil, err
	}
	if envelope.Response == nil {
		return nil, invalidResponse("hosts response field is missing")
	}
	hosts := make([]Host, 0, len(*envelope.Response))
	for index, item := range *envelope.Response {
		host, err := item.model()
		if err != nil {
			return nil, invalidResponse(fmt.Sprintf("host %d is incomplete", index))
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// GenerateSecretKey calls KeygenController_generateKey (GET /api/keygen).
// Per the supplied contract and project rule, response.pubKey is the Remnanode
// SECRET_KEY value.
func (c *Client) GenerateSecretKey(ctx context.Context) (string, error) {
	var envelope keygenEnvelope
	if err := c.doJSON(ctx, http.MethodGet, "/api/keygen", nil, http.StatusOK, &envelope); err != nil {
		return "", err
	}
	if envelope.Response == nil || envelope.Response.PubKey == nil || *envelope.Response.PubKey == "" {
		return "", invalidResponse("keygen response.pubKey is missing")
	}
	return *envelope.Response.PubKey, nil
}

// GetNodes calls NodesController_getAllNodes (GET /api/nodes).
func (c *Client) GetNodes(ctx context.Context) ([]Node, error) {
	var envelope nodesEnvelope
	if err := c.doJSON(ctx, http.MethodGet, "/api/nodes", nil, http.StatusOK, &envelope); err != nil {
		return nil, err
	}
	if envelope.Response == nil {
		return nil, invalidResponse("nodes response field is missing")
	}
	nodes := make([]Node, 0, len(*envelope.Response))
	for index, item := range *envelope.Response {
		node, err := item.model()
		if err != nil {
			return nil, invalidResponse(fmt.Sprintf("node %d is incomplete", index))
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// GetNode calls NodesController_getOneNode (GET /api/nodes/{uuid}) and is
// suitable for polling connection state.
func (c *Client) GetNode(ctx context.Context, uuid string) (Node, error) {
	uuid = strings.TrimSpace(uuid)
	if !validUUID(uuid) {
		return Node{}, fmt.Errorf("Node UUID: %w", ErrInvalidInput)
	}
	var envelope nodeEnvelope
	path := "/api/nodes/" + url.PathEscape(uuid)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, http.StatusOK, &envelope); err != nil {
		return Node{}, err
	}
	if envelope.Response == nil {
		return Node{}, invalidResponse("node response field is missing")
	}
	node, err := envelope.Response.model()
	if err != nil {
		return Node{}, invalidResponse("node response is incomplete")
	}
	return node, nil
}

// CreateNode checks current Nodes for duplicates and then calls
// NodesController_createNode (POST /api/nodes). Port is always 2222 and config
// profile values are derived only from input.Host.
func (c *Client) CreateNode(ctx context.Context, input CreateNodeInput) (Node, error) {
	name := strings.TrimSpace(input.Name)
	if !validNodeName(name) || !input.Address.IsValid() {
		return Node{}, fmt.Errorf("Node name or address: %w", ErrInvalidInput)
	}
	profile, err := DeploymentProfileFromHost(input.Host)
	if err != nil {
		return Node{}, err
	}
	existing, err := c.GetNodes(ctx)
	if err != nil {
		return Node{}, fmt.Errorf("check existing Nodes: %w", err)
	}
	if err := CheckNodeDuplicates(existing, name, input.Address); err != nil {
		return Node{}, err
	}

	request := createNodeRequest{
		Name:    name,
		Address: input.Address.String(),
		Port:    nodePort,
		ConfigProfile: NodeConfigProfile{
			ActiveConfigProfileUUID: profile.ActiveConfigProfileUUID,
			ActiveInbounds:          profile.ActiveInbounds,
		},
	}
	var envelope nodeEnvelope
	if err := c.doJSON(ctx, http.MethodPost, "/api/nodes", request, http.StatusCreated, &envelope); err != nil {
		return Node{}, err
	}
	if envelope.Response == nil {
		return Node{}, invalidResponse("created node response field is missing")
	}
	node, err := envelope.Response.model()
	if err != nil {
		return Node{}, invalidResponse("created node response is incomplete")
	}
	return node, nil
}

// UpdateNodeAddress calls NodesController_updateNode (PATCH /api/nodes) and
// sends only the UUID plus the new address, as allowed by UpdateNodeRequestDto.
func (c *Client) UpdateNodeAddress(ctx context.Context, input UpdateNodeAddressInput) (Node, error) {
	uuid := strings.TrimSpace(input.UUID)
	if !validUUID(uuid) || !input.Address.IsValid() {
		return Node{}, fmt.Errorf("Node UUID or address: %w", ErrInvalidInput)
	}
	var envelope nodeEnvelope
	request := updateNodeAddressRequest{UUID: uuid, Address: input.Address.Unmap().String()}
	if err := c.doJSON(ctx, http.MethodPatch, "/api/nodes", request, http.StatusOK, &envelope); err != nil {
		return Node{}, err
	}
	if envelope.Response == nil {
		return Node{}, invalidResponse("updated node response field is missing")
	}
	node, err := envelope.Response.model()
	if err != nil {
		return Node{}, invalidResponse("updated node response is incomplete")
	}
	return node, nil
}

// UpdateNodeProfile calls NodesController_updateNode (PATCH /api/nodes) with
// the configProfile contract derived from a fresh, validated Host.
func (c *Client) UpdateNodeProfile(ctx context.Context, input UpdateNodeProfileInput) (Node, error) {
	uuid := strings.TrimSpace(input.UUID)
	profile, err := DeploymentProfileFromHost(input.Host)
	if !validUUID(uuid) || err != nil || input.Host.IsDisabled {
		return Node{}, fmt.Errorf("Node UUID or Host profile: %w", ErrInvalidInput)
	}
	request := updateNodeProfileRequest{
		UUID: uuid,
		ConfigProfile: NodeConfigProfile{
			ActiveConfigProfileUUID: profile.ActiveConfigProfileUUID,
			ActiveInbounds:          append([]string(nil), profile.ActiveInbounds...),
		},
	}
	var envelope nodeEnvelope
	if err := c.doJSON(ctx, http.MethodPatch, "/api/nodes", request, http.StatusOK, &envelope); err != nil {
		return Node{}, err
	}
	if envelope.Response == nil {
		return Node{}, invalidResponse("updated node response field is missing")
	}
	node, err := envelope.Response.model()
	if err != nil {
		return Node{}, invalidResponse("updated node response is incomplete")
	}
	return node, nil
}

// DeleteNode calls NodesController_deleteNode (DELETE /api/nodes/{uuid}).
func (c *Client) DeleteNode(ctx context.Context, uuid string) (bool, error) {
	uuid = strings.TrimSpace(uuid)
	if !validUUID(uuid) {
		return false, fmt.Errorf("Node UUID: %w", ErrInvalidInput)
	}
	var envelope deleteNodeEnvelope
	path := "/api/nodes/" + url.PathEscape(uuid)
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, http.StatusOK, &envelope); err != nil {
		return false, err
	}
	if envelope.Response == nil || envelope.Response.IsDeleted == nil {
		return false, invalidResponse("delete response.isDeleted is missing")
	}
	return *envelope.Response.IsDeleted, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, requestBody any, expectedStatus int, response any) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode Remnawave request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}

	endpoint := c.baseURL.ResolveReference(&url.URL{Path: path})
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return fmt.Errorf("create Remnawave request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Bearer "+c.token)
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	httpResponse, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("perform Remnawave request: %w", err)
	}
	defer httpResponse.Body.Close()

	if httpResponse.StatusCode == http.StatusUnauthorized || httpResponse.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	if httpResponse.StatusCode != expectedStatus {
		return &HTTPError{StatusCode: httpResponse.StatusCode}
	}

	decoder := json.NewDecoder(io.LimitReader(httpResponse.Body, maxResponseSize))
	if err := decoder.Decode(response); err != nil {
		return invalidResponse("response body is not valid JSON")
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return invalidResponse("response body contains multiple JSON values")
	}
	return nil
}

// HTTPError represents an unexpected HTTP status without exposing response
// bodies, request headers, or bearer credentials.
type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("Remnawave API returned HTTP %d", e.StatusCode)
}

func invalidResponse(reason string) error {
	return fmt.Errorf("%s: %w", reason, ErrInvalidResponse)
}
