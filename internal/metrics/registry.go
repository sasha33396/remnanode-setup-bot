// Package metrics exposes a small Prometheus-compatible registry without
// adding a runtime dependency or accepting unbounded user-controlled labels.
package metrics

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"remnanode-setup-bot/internal/dnsbalancer"
	"remnanode-setup-bot/internal/remnawave"
)

type Registry struct {
	deploymentsTotal           atomic.Uint64
	deploymentsFailedTotal     atomic.Uint64
	activeDeployments          atomic.Int64
	remnawaveAPIErrors         atomic.Uint64
	dnsAPIErrors               atomic.Uint64
	certificateRenewalFailures atomic.Uint64

	mu                        sync.Mutex
	deploymentDurationCount   uint64
	deploymentDurationSum     float64
	provisioningDurationCount uint64
	provisioningDurationSum   float64
	certificateExpiryDays     map[string]float64
}

func New() *Registry { return &Registry{certificateExpiryDays: make(map[string]float64)} }

func (r *Registry) DeploymentCreated()              { r.deploymentsTotal.Add(1) }
func (r *Registry) DeploymentFailed()               { r.deploymentsFailedTotal.Add(1) }
func (r *Registry) ActiveDeployment(delta int64)    { r.activeDeployments.Add(delta) }
func (r *Registry) RemnawaveAPIError()              { r.remnawaveAPIErrors.Add(1) }
func (r *Registry) DNSAPIError()                    { r.dnsAPIErrors.Add(1) }
func (r *Registry) CertificateRenewalFailed(string) { r.certificateRenewalFailures.Add(1) }

func (r *Registry) DeploymentDuration(duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deploymentDurationCount++
	r.deploymentDurationSum += duration.Seconds()
}

func (r *Registry) ProvisioningStepDuration(_ string, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.provisioningDurationCount++
	r.provisioningDurationSum += duration.Seconds()
}

func (r *Registry) SetCertificateExpiry(sni string, duration time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.certificateExpiryDays[escapeLabel(sni)] = duration.Hours() / 24
}

func (r *Registry) ServeHTTP(writer http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	deploymentCount, deploymentSum := r.deploymentDurationCount, r.deploymentDurationSum
	provisioningCount, provisioningSum := r.provisioningDurationCount, r.provisioningDurationSum
	expiry := make(map[string]float64, len(r.certificateExpiryDays))
	for sni, days := range r.certificateExpiryDays {
		expiry[sni] = days
	}
	r.mu.Unlock()
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(writer, "deployments_total %d\n", r.deploymentsTotal.Load())
	_, _ = fmt.Fprintf(writer, "deployments_failed_total %d\n", r.deploymentsFailedTotal.Load())
	_, _ = fmt.Fprintf(writer, "active_deployments %d\n", r.activeDeployments.Load())
	_, _ = fmt.Fprintf(writer, "deployment_duration_seconds_count %d\n", deploymentCount)
	_, _ = fmt.Fprintf(writer, "deployment_duration_seconds_sum %g\n", deploymentSum)
	_, _ = fmt.Fprintf(writer, "provisioning_step_duration_seconds_count %d\n", provisioningCount)
	_, _ = fmt.Fprintf(writer, "provisioning_step_duration_seconds_sum %g\n", provisioningSum)
	_, _ = fmt.Fprintf(writer, "remnawave_api_errors_total %d\n", r.remnawaveAPIErrors.Load())
	_, _ = fmt.Fprintf(writer, "dns_api_errors_total %d\n", r.dnsAPIErrors.Load())
	_, _ = fmt.Fprintf(writer, "certificate_renewal_failures_total %d\n", r.certificateRenewalFailures.Load())
	keys := make([]string, 0, len(expiry))
	for key := range expiry {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		_, _ = fmt.Fprintf(writer, "certificate_expiry_days{sni=\"%s\"} %g\n", key, expiry[key])
	}
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	value = strings.ReplaceAll(value, "\r", `\n`)
	return value
}

type RemnawaveClient struct {
	next interface {
		GetHosts(context.Context) ([]remnawave.Host, error)
		GenerateSecretKey(context.Context) (string, error)
		GetNodes(context.Context) ([]remnawave.Node, error)
		GetNode(context.Context, string) (remnawave.Node, error)
		CreateNode(context.Context, remnawave.CreateNodeInput) (remnawave.Node, error)
		UpdateNodeAddress(context.Context, remnawave.UpdateNodeAddressInput) (remnawave.Node, error)
	}
	metrics *Registry
}

func ObserveRemnawave(next interface {
	GetHosts(context.Context) ([]remnawave.Host, error)
	GenerateSecretKey(context.Context) (string, error)
	GetNodes(context.Context) ([]remnawave.Node, error)
	GetNode(context.Context, string) (remnawave.Node, error)
	CreateNode(context.Context, remnawave.CreateNodeInput) (remnawave.Node, error)
	UpdateNodeAddress(context.Context, remnawave.UpdateNodeAddressInput) (remnawave.Node, error)
}, registry *Registry) *RemnawaveClient {
	return &RemnawaveClient{next: next, metrics: registry}
}

func (c *RemnawaveClient) GetHosts(ctx context.Context) ([]remnawave.Host, error) {
	value, err := c.next.GetHosts(ctx)
	c.observe(err)
	return value, err
}
func (c *RemnawaveClient) GenerateSecretKey(ctx context.Context) (string, error) {
	value, err := c.next.GenerateSecretKey(ctx)
	c.observe(err)
	return value, err
}
func (c *RemnawaveClient) GetNodes(ctx context.Context) ([]remnawave.Node, error) {
	value, err := c.next.GetNodes(ctx)
	c.observe(err)
	return value, err
}
func (c *RemnawaveClient) GetNode(ctx context.Context, id string) (remnawave.Node, error) {
	value, err := c.next.GetNode(ctx, id)
	c.observe(err)
	return value, err
}
func (c *RemnawaveClient) CreateNode(ctx context.Context, input remnawave.CreateNodeInput) (remnawave.Node, error) {
	value, err := c.next.CreateNode(ctx, input)
	c.observe(err)
	return value, err
}
func (c *RemnawaveClient) UpdateNodeAddress(ctx context.Context, input remnawave.UpdateNodeAddressInput) (remnawave.Node, error) {
	value, err := c.next.UpdateNodeAddress(ctx, input)
	c.observe(err)
	return value, err
}
func (c *RemnawaveClient) observe(err error) {
	if err != nil && c.metrics != nil {
		c.metrics.RemnawaveAPIError()
	}
}

type DNSClient struct {
	next interface {
		FindZone(context.Context, string) (dnsbalancer.ZoneMatch, error)
		FindZonesByIP(context.Context, netip.Addr) ([]dnsbalancer.ZoneMatch, error)
		AddIP(context.Context, string, netip.Addr) (dnsbalancer.AddIPResult, error)
		ReplaceIP(context.Context, string, netip.Addr, netip.Addr) (dnsbalancer.ReplaceIPResult, error)
	}
	metrics *Registry
}

func ObserveDNS(next interface {
	FindZone(context.Context, string) (dnsbalancer.ZoneMatch, error)
	FindZonesByIP(context.Context, netip.Addr) ([]dnsbalancer.ZoneMatch, error)
	AddIP(context.Context, string, netip.Addr) (dnsbalancer.AddIPResult, error)
	ReplaceIP(context.Context, string, netip.Addr, netip.Addr) (dnsbalancer.ReplaceIPResult, error)
}, registry *Registry) *DNSClient {
	return &DNSClient{next: next, metrics: registry}
}

func (c *DNSClient) FindZone(ctx context.Context, sni string) (dnsbalancer.ZoneMatch, error) {
	value, err := c.next.FindZone(ctx, sni)
	c.observe(err)
	return value, err
}
func (c *DNSClient) FindZonesByIP(ctx context.Context, ip netip.Addr) ([]dnsbalancer.ZoneMatch, error) {
	value, err := c.next.FindZonesByIP(ctx, ip)
	c.observe(err)
	return value, err
}
func (c *DNSClient) AddIP(ctx context.Context, sni string, ip netip.Addr) (dnsbalancer.AddIPResult, error) {
	value, err := c.next.AddIP(ctx, sni, ip)
	c.observe(err)
	return value, err
}
func (c *DNSClient) ReplaceIP(ctx context.Context, sni string, oldIP, newIP netip.Addr) (dnsbalancer.ReplaceIPResult, error) {
	value, err := c.next.ReplaceIP(ctx, sni, oldIP, newIP)
	c.observe(err)
	return value, err
}
func (c *DNSClient) observe(err error) {
	if err != nil && c.metrics != nil {
		c.metrics.DNSAPIError()
	}
}
