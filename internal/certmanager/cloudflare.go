package certmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxCloudflareResponse = 2 << 20

type TXTResolver interface {
	LookupTXT(context.Context, string) ([]string, error)
}

type CloudflareDNS struct {
	baseURL             *url.URL
	token               string
	http                *http.Client
	resolver            TXTResolver
	propagationTimeout  time.Duration
	propagationInterval time.Duration
}

func NewCloudflareDNS(token string, timeout, propagationTimeout, propagationInterval time.Duration) (*CloudflareDNS, error) {
	base, _ := url.Parse("https://api.cloudflare.com/client/v4/")
	return newCloudflareDNS(base, token, timeout, propagationTimeout, propagationInterval, net.DefaultResolver)
}

func newCloudflareDNS(base *url.URL, token string, timeout, propagationTimeout, propagationInterval time.Duration, resolver TXTResolver) (*CloudflareDNS, error) {
	if base == nil || base.Scheme == "" || base.Host == "" || strings.TrimSpace(token) == "" || timeout <= 0 || propagationTimeout <= 0 || propagationInterval <= 0 || resolver == nil {
		return nil, ErrInvalidInput
	}
	return &CloudflareDNS{baseURL: base, token: strings.TrimSpace(token), http: &http.Client{Timeout: timeout}, resolver: resolver, propagationTimeout: propagationTimeout, propagationInterval: propagationInterval}, nil
}

func (c *CloudflareDNS) Present(ctx context.Context, fqdn, value string) (func(context.Context) error, error) {
	name, err := canonicalChallengeName(fqdn)
	if err != nil || strings.TrimSpace(value) == "" {
		return nil, ErrInvalidInput
	}
	zoneID, err := c.findZone(ctx, name)
	if err != nil {
		return nil, safe(SafeMessage(err, "Cloudflare DNS zone could not be resolved"), ErrIssuanceFailed)
	}
	var response cloudflareResponse[cloudflareRecord]
	err = c.call(ctx, http.MethodPost, "zones/"+url.PathEscape(zoneID)+"/dns_records", map[string]any{"type": "TXT", "name": name, "content": value, "ttl": 60}, &response)
	if err != nil {
		return nil, safe(SafeMessage(err, "Cloudflare DNS challenge record could not be created"), ErrIssuanceFailed)
	}
	if !response.Success || response.Result.ID == "" {
		return nil, cloudflareRejected("Cloudflare rejected the DNS challenge record", response.Errors)
	}
	recordID := response.Result.ID
	return func(cleanupCtx context.Context) error {
		var deleted cloudflareResponse[json.RawMessage]
		if err := c.call(cleanupCtx, http.MethodDelete, "zones/"+url.PathEscape(zoneID)+"/dns_records/"+url.PathEscape(recordID), nil, &deleted); err != nil {
			return safe(SafeMessage(err, "Cloudflare DNS challenge record could not be removed"), ErrIssuanceFailed)
		}
		if !deleted.Success {
			return cloudflareRejected("Cloudflare rejected DNS challenge cleanup", deleted.Errors)
		}
		return nil
	}, nil
}

func (c *CloudflareDNS) WaitPropagation(ctx context.Context, fqdn, value string) error {
	waitCtx, cancel := context.WithTimeout(ctx, c.propagationTimeout)
	defer cancel()
	ticker := time.NewTicker(c.propagationInterval)
	defer ticker.Stop()
	for {
		values, err := c.resolver.LookupTXT(waitCtx, fqdn)
		if err == nil {
			for _, found := range values {
				if found == value {
					return nil
				}
			}
		}
		select {
		case <-waitCtx.Done():
			return safe("ACME DNS challenge record did not propagate before timeout", ErrIssuanceFailed)
		case <-ticker.C:
		}
	}
}

func (c *CloudflareDNS) findZone(ctx context.Context, fqdn string) (string, error) {
	labels := strings.Split(fqdn, ".")
	for index := 0; index < len(labels)-1; index++ {
		candidate := strings.Join(labels[index:], ".")
		query := url.Values{"name": []string{candidate}, "status": []string{"active"}}
		var response cloudflareResponse[[]cloudflareZone]
		if err := c.call(ctx, http.MethodGet, "zones?"+query.Encode(), nil, &response); err != nil {
			return "", err
		}
		if !response.Success {
			return "", cloudflareRejected("Cloudflare rejected DNS zone lookup", response.Errors)
		}
		if response.Success && len(response.Result) == 1 && response.Result[0].ID != "" {
			return response.Result[0].ID, nil
		}
	}
	return "", safe("Cloudflare DNS zone was not found for the SNI", ErrNotFound)
}

func (c *CloudflareDNS) call(ctx context.Context, method, path string, body any, target any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return ErrIssuanceFailed
		}
		reader = bytes.NewReader(encoded)
	}
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: strings.SplitN(path, "?", 2)[0]})
	if parts := strings.SplitN(path, "?", 2); len(parts) == 2 {
		endpoint.RawQuery = parts[1]
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return ErrIssuanceFailed
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return safe("Cloudflare API request failed", ErrIssuanceFailed)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		switch response.StatusCode {
		case http.StatusUnauthorized:
			return safe("Cloudflare API authentication failed (HTTP 401)", ErrIssuanceFailed)
		case http.StatusForbidden:
			return safe("Cloudflare API token lacks DNS permissions (HTTP 403)", ErrIssuanceFailed)
		case http.StatusTooManyRequests:
			return safe("Cloudflare API rate limit was reached (HTTP 429)", ErrIssuanceFailed)
		default:
			return safe(fmt.Sprintf("Cloudflare API request failed (HTTP %d)", response.StatusCode), ErrIssuanceFailed)
		}
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxCloudflareResponse))
	if err := decoder.Decode(target); err != nil {
		return ErrIssuanceFailed
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrIssuanceFailed
	}
	return nil
}

func canonicalChallengeName(value string) (string, error) {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if !strings.HasPrefix(value, "_acme-challenge.") {
		return "", ErrInvalidInput
	}
	if _, err := canonicalSNI(strings.TrimPrefix(value, "_acme-challenge.")); err != nil {
		return "", err
	}
	return value, nil
}

type cloudflareResponse[T any] struct {
	Success bool `json:"success"`
	Result  T    `json:"result"`
	Errors  []cloudflareProblem `json:"errors"`
}

type cloudflareProblem struct {
	Code int `json:"code"`
}

func cloudflareRejected(message string, problems []cloudflareProblem) error {
	if len(problems) > 0 && problems[0].Code > 0 {
		message = fmt.Sprintf("%s (code %d)", message, problems[0].Code)
	}
	return safe(message, ErrIssuanceFailed)
}

type cloudflareZone struct {
	ID string `json:"id"`
}
type cloudflareRecord struct {
	ID string `json:"id"`
}

func (*CloudflareDNS) String() string   { return "CloudflareDNS{token:[REDACTED]}" }
func (*CloudflareDNS) GoString() string { return "CloudflareDNS{token:[REDACTED]}" }

var _ DNSChallengeProvider = (*CloudflareDNS)(nil)
