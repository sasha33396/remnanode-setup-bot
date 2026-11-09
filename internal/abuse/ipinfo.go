package abuse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const ipInfoMaxResponseSize = 1 << 20

// IPInfoClient resolves ASN/provider data on the server. Tokens are never
// included in returned errors or logs.
type IPInfoClient struct {
	baseURL *url.URL
	token   string
	tier    string
	http    *http.Client
}

func NewIPInfoClient(token, tier string, timeout time.Duration) (*IPInfoClient, error) {
	base, _ := url.Parse("https://api.ipinfo.io/")
	if timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	return newIPInfoClient(base, token, tier, &http.Client{Timeout: timeout})
}

func newIPInfoClient(base *url.URL, token, tier string, client *http.Client) (*IPInfoClient, error) {
	token, tier = strings.TrimSpace(token), strings.ToLower(strings.TrimSpace(tier))
	if token == "" || (tier != "lite" && tier != "core") || base == nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" || client == nil || client.Timeout <= 0 {
		return nil, errors.New("invalid IPinfo configuration")
	}
	return &IPInfoClient{baseURL: base, token: token, tier: tier, http: client}, nil
}

func (c *IPInfoClient) Lookup(ctx context.Context, ip netip.Addr) (Intelligence, error) {
	if !ip.IsValid() || !ip.IsGlobalUnicast() {
		return Intelligence{}, errors.New("invalid IPinfo address")
	}
	endpointName := "lite"
	if c.tier == "core" {
		endpointName = "lookup"
	}
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: endpointName + "/" + ip.Unmap().String()})
	query := endpoint.Query()
	query.Set("token", c.token)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return Intelligence{}, errors.New("create IPinfo request failed")
	}
	request.Header.Set("Accept", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return Intelligence{}, errors.New("perform IPinfo request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Intelligence{}, errors.New("IPinfo API returned HTTP " + strconv.Itoa(response.StatusCode))
	}
	var payload struct {
		IP       string `json:"ip"`
		ASN      string `json:"asn"`
		ASName   string `json:"as_name"`
		ASDomain string `json:"as_domain"`
		AS       struct {
			ASN, Name, Domain, Type string
		} `json:"as"`
		IsAnonymous bool `json:"is_anonymous"`
		IsHosting   bool `json:"is_hosting"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, ipInfoMaxResponseSize))
	if err := decoder.Decode(&payload); err != nil {
		return Intelligence{}, errors.New("decode IPinfo response failed")
	}
	if parsed, parseErr := netip.ParseAddr(payload.IP); parseErr != nil || parsed.Unmap() != ip.Unmap() {
		return Intelligence{}, errors.New("invalid IPinfo response")
	}
	asnText, provider, category := payload.ASN, payload.ASName, "UNKNOWN"
	if c.tier == "core" {
		asnText, provider = payload.AS.ASN, payload.AS.Name
		category = strings.ToUpper(strings.TrimSpace(payload.AS.Type))
		if payload.IsHosting {
			category = "HOSTING"
		}
		if payload.IsAnonymous {
			category = "PROXY"
		}
	}
	asnText = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(asnText)), "AS")
	asn, parseErr := strconv.ParseUint(asnText, 10, 32)
	if parseErr != nil || asn == 0 {
		return Intelligence{}, errors.New("invalid IPinfo ASN")
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "UNKNOWN"
	}
	if category == "" {
		category = "UNKNOWN"
	}
	return Intelligence{ASN: uint32(asn), Provider: provider, Category: category}, nil
}
