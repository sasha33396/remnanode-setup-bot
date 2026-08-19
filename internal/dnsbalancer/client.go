package dnsbalancer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

const maxResponseSize = 4 << 20

var (
	// ErrUnauthorized represents HTTP 401.
	ErrUnauthorized = errors.New("DNS-balancer authorization failed")
	// ErrConflict represents HTTP 409.
	ErrConflict = errors.New("DNS-balancer conflict")
	// ErrUnprocessable represents HTTP 422.
	ErrUnprocessable = errors.New("DNS-balancer validation failed")
	// ErrInvalidResponse indicates malformed or incomplete response JSON.
	ErrInvalidResponse = errors.New("invalid DNS-balancer API response")
)

// API is the deployment-facing DNS-balancer behavior.
type API interface {
	GetDomains(context.Context) ([]Domain, error)
	FindZone(context.Context, string) (ZoneMatch, error)
	FindZonesByIP(context.Context, netip.Addr) ([]ZoneMatch, error)
	AddIP(context.Context, string, netip.Addr) (AddIPResult, error)
	ReplaceIP(context.Context, string, netip.Addr, netip.Addr) (ReplaceIPResult, error)
	MoveIP(context.Context, string, string, netip.Addr) (MoveIPResult, error)
}

// FindZonesByIP returns every simple or advanced zone containing ip.
func (c *Client) FindZonesByIP(ctx context.Context, ip netip.Addr) ([]ZoneMatch, error) {
	if !ip.IsValid() {
		return nil, fmt.Errorf("IP: %w", ErrInvalidInput)
	}
	domains, err := c.GetDomains(ctx)
	if err != nil {
		return nil, err
	}
	ip = ip.Unmap()
	result := make([]ZoneMatch, 0)
	for _, domain := range domains {
		for _, zone := range domain.Zones {
			matched := false
			for _, value := range zone.IPs {
				parsed, parseErr := netip.ParseAddr(strings.TrimSpace(value))
				if parseErr == nil && parsed.Unmap() == ip {
					matched = true
					break
				}
			}
			if !matched {
				for _, node := range zone.Nodes {
					parsed, parseErr := netip.ParseAddr(strings.TrimSpace(node.IP))
					if parseErr == nil && parsed.Unmap() == ip {
						matched = true
						break
					}
				}
			}
			if matched {
				fqdn := zone.Name + "." + domain.Name
				if zone.Name == "@" {
					fqdn = domain.Name
				}
				result = append(result, ZoneMatch{Domain: domain.Name, ZoneName: zone.Name, FQDN: normalizeFQDN(fqdn), Zone: zone})
			}
		}
	}
	return result, nil
}

// Client is a typed remnawave-cloudflare-nodes HTTP client.
type Client struct {
	baseURL *url.URL
	apiKey  string
	http    *http.Client
	locker  ZoneLocker
}

var _ API = (*Client)(nil)

// NewClient creates a client with an explicit timeout. A nil locker selects a
// process-local keyed mutex; callers may inject another ZoneLocker later.
func NewClient(baseURL, apiKey string, timeout time.Duration, locker ZoneLocker) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("base URL: %w", ErrInvalidInput)
	}
	if strings.TrimSpace(apiKey) == "" {
		return nil, fmt.Errorf("API key: %w", ErrInvalidInput)
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("HTTP timeout: %w", ErrInvalidInput)
	}
	if locker == nil {
		locker = NewMemoryZoneLocker()
	}
	return &Client{
		baseURL: parsed,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: timeout},
		locker:  locker,
	}, nil
}

// GetDomains calls GET /api/config/domains.
func (c *Client) GetDomains(ctx context.Context) ([]Domain, error) {
	var response []domainWire
	if err := c.doJSON(ctx, http.MethodGet, c.endpoint("/api/config/domains"), nil, http.StatusOK, &response); err != nil {
		return nil, err
	}
	if response == nil {
		return nil, invalidResponse("domains response must be an array")
	}
	domains := make([]Domain, 0, len(response))
	for index, item := range response {
		domain, err := item.model()
		if err != nil {
			return nil, invalidResponse(fmt.Sprintf("domain %d is incomplete", index))
		}
		domains = append(domains, domain)
	}
	return domains, nil
}

// FindZone refreshes domain configuration and locates an exact FQDN.
func (c *Client) FindZone(ctx context.Context, fqdn string) (ZoneMatch, error) {
	domains, err := c.GetDomains(ctx)
	if err != nil {
		return ZoneMatch{}, err
	}
	return LocateZone(domains, fqdn)
}

// AddIP serializes a complete read-modify-write cycle for fqdn. PATCH receives
// every existing simple IP plus the new IP, never only the new value.
func (c *Client) AddIP(ctx context.Context, fqdn string, ip netip.Addr) (AddIPResult, error) {
	key := normalizeFQDN(fqdn)
	if key == "" || !ip.IsValid() {
		return AddIPResult{}, fmt.Errorf("FQDN or IP: %w", ErrInvalidInput)
	}
	ip = ip.Unmap()

	unlock, err := c.locker.Lock(ctx, key)
	if err != nil {
		return AddIPResult{}, fmt.Errorf("lock DNS zone: %w", err)
	}
	defer unlock()

	domains, err := c.GetDomains(ctx)
	if err != nil {
		return AddIPResult{}, fmt.Errorf("read DNS configuration: %w", err)
	}
	match, err := LocateZone(domains, key)
	if err != nil {
		return AddIPResult{}, err
	}

	complete := append([]string(nil), match.Zone.IPs...)
	for _, existing := range complete {
		parsed, parseErr := netip.ParseAddr(strings.TrimSpace(existing))
		if parseErr != nil {
			return AddIPResult{}, invalidResponse("zone contains an invalid IP")
		}
		if parsed.Unmap() == ip {
			return AddIPResult{FQDN: match.FQDN, Added: false, IPs: complete}, nil
		}
	}
	for _, existing := range match.Zone.Nodes {
		parsed, parseErr := netip.ParseAddr(strings.TrimSpace(existing.IP))
		if parseErr != nil {
			return AddIPResult{}, invalidResponse("zone contains an invalid advanced-node IP")
		}
		if parsed.Unmap() == ip {
			return AddIPResult{FQDN: match.FQDN, Added: false, IPs: complete}, nil
		}
	}
	complete = append(complete, ip.String())

	var response statusResponse
	endpoint := c.zoneEndpoint(match.Domain, match.ZoneName)
	if err := c.doJSON(ctx, http.MethodPatch, endpoint, patchZoneRequest{IPs: complete}, http.StatusOK, &response); err != nil {
		return AddIPResult{}, err
	}
	if response.Status == nil || *response.Status != "ok" {
		return AddIPResult{}, invalidResponse("PATCH status is missing or not ok")
	}
	return AddIPResult{FQDN: match.FQDN, Added: true, IPs: complete}, nil
}

// ReplaceIP atomically replaces an IP in either supported zone format while
// preserving every other entry. A completed replacement is a no-op.
func (c *Client) ReplaceIP(ctx context.Context, fqdn string, oldIP, newIP netip.Addr) (ReplaceIPResult, error) {
	key := normalizeFQDN(fqdn)
	if key == "" || !oldIP.IsValid() || !newIP.IsValid() || oldIP.Unmap() == newIP.Unmap() {
		return ReplaceIPResult{}, fmt.Errorf("FQDN or IP: %w", ErrInvalidInput)
	}
	oldIP, newIP = oldIP.Unmap(), newIP.Unmap()
	unlock, err := c.locker.Lock(ctx, key)
	if err != nil {
		return ReplaceIPResult{}, fmt.Errorf("lock DNS zone: %w", err)
	}
	defer unlock()
	domains, err := c.GetDomains(ctx)
	if err != nil {
		return ReplaceIPResult{}, fmt.Errorf("read DNS configuration: %w", err)
	}
	match, err := LocateZone(domains, key)
	if err != nil {
		return ReplaceIPResult{}, err
	}
	if len(match.Zone.Nodes) != 0 {
		foundOld, foundNew := false, false
		for _, item := range match.Zone.Nodes {
			parsed, parseErr := netip.ParseAddr(strings.TrimSpace(item.IP))
			if parseErr != nil {
				return ReplaceIPResult{}, invalidResponse("zone contains an invalid advanced-node IP")
			}
			switch parsed.Unmap() {
			case oldIP:
				foundOld = true
			case newIP:
				foundNew = true
			}
		}
		if !foundOld {
			if foundNew {
				return ReplaceIPResult{FQDN: match.FQDN, Nodes: append([]ZoneNode(nil), match.Zone.Nodes...)}, nil
			}
			return ReplaceIPResult{}, fmt.Errorf("old IP is absent: %w", ErrNotFound)
		}
		complete := make([]ZoneNode, 0, len(match.Zone.Nodes))
		for _, item := range match.Zone.Nodes {
			parsed, _ := netip.ParseAddr(strings.TrimSpace(item.IP))
			if parsed.Unmap() == oldIP {
				if foundNew {
					continue
				}
				item.IP = newIP.String()
			}
			complete = append(complete, item)
		}
		var response statusResponse
		if err := c.doJSON(ctx, http.MethodPatch, c.zoneEndpoint(match.Domain, match.ZoneName), patchZoneRequest{Nodes: complete}, http.StatusOK, &response); err != nil {
			return ReplaceIPResult{}, err
		}
		if response.Status == nil || *response.Status != "ok" {
			return ReplaceIPResult{}, invalidResponse("PATCH status is missing or not ok")
		}
		return ReplaceIPResult{FQDN: match.FQDN, Changed: true, Nodes: complete}, nil
	}
	complete := make([]string, 0, len(match.Zone.IPs))
	foundOld, foundNew := false, false
	for _, value := range match.Zone.IPs {
		parsed, parseErr := netip.ParseAddr(strings.TrimSpace(value))
		if parseErr != nil {
			return ReplaceIPResult{}, invalidResponse("zone contains an invalid IP")
		}
		switch parsed.Unmap() {
		case oldIP:
			foundOld = true
		case newIP:
			foundNew = true
			complete = append(complete, newIP.String())
		default:
			complete = append(complete, parsed.Unmap().String())
		}
	}
	if !foundOld {
		if foundNew {
			return ReplaceIPResult{FQDN: match.FQDN, IPs: complete}, nil
		}
		return ReplaceIPResult{}, fmt.Errorf("old IP is absent: %w", ErrNotFound)
	}
	if !foundNew {
		complete = append(complete, newIP.String())
	}
	var response statusResponse
	if err := c.doJSON(ctx, http.MethodPatch, c.zoneEndpoint(match.Domain, match.ZoneName), patchZoneRequest{IPs: complete}, http.StatusOK, &response); err != nil {
		return ReplaceIPResult{}, err
	}
	if response.Status == nil || *response.Status != "ok" {
		return ReplaceIPResult{}, invalidResponse("PATCH status is missing or not ok")
	}
	return ReplaceIPResult{FQDN: match.FQDN, Changed: true, IPs: complete}, nil
}

// MoveIP transfers ip between two zones while holding both zone locks. The
// target is patched first; a source PATCH failure restores the target snapshot.
func (c *Client) MoveIP(ctx context.Context, sourceFQDN, targetFQDN string, ip netip.Addr) (MoveIPResult, error) {
	sourceKey, targetKey := normalizeFQDN(sourceFQDN), normalizeFQDN(targetFQDN)
	if sourceKey == "" || targetKey == "" || sourceKey == targetKey || !ip.IsValid() {
		return MoveIPResult{}, fmt.Errorf("FQDN or IP: %w", ErrInvalidInput)
	}
	keys := []string{sourceKey, targetKey}
	if keys[1] < keys[0] {
		keys[0], keys[1] = keys[1], keys[0]
	}
	unlockFirst, err := c.locker.Lock(ctx, keys[0])
	if err != nil {
		return MoveIPResult{}, fmt.Errorf("lock DNS zone: %w", err)
	}
	defer unlockFirst()
	unlockSecond, err := c.locker.Lock(ctx, keys[1])
	if err != nil {
		return MoveIPResult{}, fmt.Errorf("lock DNS zone: %w", err)
	}
	defer unlockSecond()

	domains, err := c.GetDomains(ctx)
	if err != nil {
		return MoveIPResult{}, fmt.Errorf("read DNS configuration: %w", err)
	}
	source, err := LocateZone(domains, sourceKey)
	if err != nil {
		return MoveIPResult{}, err
	}
	target, err := LocateZone(domains, targetKey)
	if err != nil {
		return MoveIPResult{}, err
	}
	if (source.Zone.Nodes != nil) != (target.Zone.Nodes != nil) {
		return MoveIPResult{}, fmt.Errorf("source and target zone formats differ: %w", ErrInvalidInput)
	}
	ip = ip.Unmap()
	sourceRequest, sourceFound, sourceNode, err := zoneWithoutIP(source.Zone, ip)
	if err != nil {
		return MoveIPResult{}, err
	}
	targetRequest, targetFound, err := zoneWithIP(target.Zone, ip, sourceNode)
	if err != nil {
		return MoveIPResult{}, err
	}
	result := MoveIPResult{SourceFQDN: source.FQDN, TargetFQDN: target.FQDN}
	if !sourceFound {
		if targetFound {
			return result, nil
		}
		return MoveIPResult{}, fmt.Errorf("source IP is absent: %w", ErrNotFound)
	}
	if !targetFound {
		if err := c.patchZone(ctx, target, targetRequest); err != nil {
			return MoveIPResult{}, err
		}
	}
	if err := c.patchZone(ctx, source, sourceRequest); err != nil {
		if !targetFound {
			_ = c.patchZone(ctx, target, zoneSnapshotRequest(target.Zone))
		}
		return MoveIPResult{}, err
	}
	result.Changed = true
	return result, nil
}

func zoneWithoutIP(zone Zone, ip netip.Addr) (patchZoneRequest, bool, *ZoneNode, error) {
	if zone.Nodes != nil {
		nodes := make([]ZoneNode, 0, len(zone.Nodes))
		var moved *ZoneNode
		for _, item := range zone.Nodes {
			parsed, err := netip.ParseAddr(strings.TrimSpace(item.IP))
			if err != nil {
				return patchZoneRequest{}, false, nil, invalidResponse("zone contains an invalid advanced-node IP")
			}
			if parsed.Unmap() == ip {
				copy := item
				moved = &copy
				continue
			}
			nodes = append(nodes, item)
		}
		return patchZoneRequest{Nodes: nodes}, moved != nil, moved, nil
	}
	ips := make([]string, 0, len(zone.IPs))
	found := false
	for _, value := range zone.IPs {
		parsed, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil {
			return patchZoneRequest{}, false, nil, invalidResponse("zone contains an invalid IP")
		}
		if parsed.Unmap() == ip {
			found = true
			continue
		}
		ips = append(ips, parsed.Unmap().String())
	}
	return patchZoneRequest{IPs: ips}, found, nil, nil
}

func zoneWithIP(zone Zone, ip netip.Addr, sourceNode *ZoneNode) (patchZoneRequest, bool, error) {
	if zone.Nodes != nil {
		nodes := append([]ZoneNode(nil), zone.Nodes...)
		for _, item := range nodes {
			parsed, err := netip.ParseAddr(strings.TrimSpace(item.IP))
			if err != nil {
				return patchZoneRequest{}, false, invalidResponse("zone contains an invalid advanced-node IP")
			}
			if parsed.Unmap() == ip {
				return patchZoneRequest{Nodes: nodes}, true, nil
			}
		}
		if sourceNode == nil {
			return patchZoneRequest{}, false, fmt.Errorf("cannot infer advanced-node address: %w", ErrInvalidInput)
		}
		nodes = append(nodes, *sourceNode)
		return patchZoneRequest{Nodes: nodes}, false, nil
	}
	ips := append([]string(nil), zone.IPs...)
	for _, value := range ips {
		parsed, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil {
			return patchZoneRequest{}, false, invalidResponse("zone contains an invalid IP")
		}
		if parsed.Unmap() == ip {
			return patchZoneRequest{IPs: ips}, true, nil
		}
	}
	ips = append(ips, ip.String())
	return patchZoneRequest{IPs: ips}, false, nil
}

func zoneSnapshotRequest(zone Zone) patchZoneRequest {
	if zone.Nodes != nil {
		return patchZoneRequest{Nodes: append([]ZoneNode(nil), zone.Nodes...)}
	}
	return patchZoneRequest{IPs: append([]string(nil), zone.IPs...)}
}

func (c *Client) patchZone(ctx context.Context, match ZoneMatch, request patchZoneRequest) error {
	var response statusResponse
	if err := c.doJSON(ctx, http.MethodPatch, c.zoneEndpoint(match.Domain, match.ZoneName), request, http.StatusOK, &response); err != nil {
		return err
	}
	if response.Status == nil || *response.Status != "ok" {
		return invalidResponse("PATCH status is missing or not ok")
	}
	return nil
}

func (c *Client) endpoint(path string) *url.URL {
	return c.baseURL.ResolveReference(&url.URL{Path: path})
}

func (c *Client) zoneEndpoint(domain, zone string) *url.URL {
	result := *c.baseURL
	result.Path = "/api/config/domains/" + domain + "/zones/" + zone
	escapedZone := url.PathEscape(zone)
	if zone == "@" {
		escapedZone = "%40"
	}
	result.RawPath = "/api/config/domains/" + url.PathEscape(domain) + "/zones/" + escapedZone
	return &result
}

func (c *Client) doJSON(ctx context.Context, method string, endpoint *url.URL, requestBody any, expectedStatus int, response any) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("encode DNS-balancer request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return fmt.Errorf("create DNS-balancer request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-API-Key", c.apiKey)
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	httpResponse, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("perform DNS-balancer request: %w", err)
	}
	defer httpResponse.Body.Close()
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

// HTTPError contains only an HTTP status and never the X-API-Key or response
// body. Known statuses unwrap to stable sentinel errors.
type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("DNS-balancer API returned HTTP %d", e.StatusCode)
}

func (e *HTTPError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrConflict
	case http.StatusUnprocessableEntity:
		return ErrUnprocessable
	default:
		return nil
	}
}

func invalidResponse(reason string) error {
	return fmt.Errorf("%s: %w", reason, ErrInvalidResponse)
}
