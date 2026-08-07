// Package config loads and validates application configuration.
package config

import (
	"errors"
	"fmt"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHealthAddr       = ":8080"
	defaultXraySNIRepoURL   = "https://github.com/sasha33396/sni-external.git"
	defaultXraySNIRef       = "v0.1.0-external"
	defaultACMEDirectory    = "https://acme-v02.api.letsencrypt.org/directory"
	defaultCertificateStore = "/var/lib/deployer/certificates"
)

// Config contains the deployer's startup configuration. Callers must not log
// the complete value because it contains secrets.
type Config struct {
	TelegramBotToken            string
	TelegramAllowedUsers        []int64
	RemnawaveURL                string
	RemnawaveToken              string
	DNSBalancerURL              string
	DNSBalancerToken            string
	CloudflareAPIToken          string
	ACMEEmail                   string
	ACMEDirectoryURL            string
	CertificateStorePath        string
	RemnaAPIIP                  netip.Addr
	MetricsIP                   netip.Addr
	DatabaseURL                 string
	DeploySSHPrivateKey         string
	XraySNIRepoURL              string
	XraySNIRef                  string
	HealthAddr                  string
	HTTPTimeout                 time.Duration
	SSHConnectTimeout           time.Duration
	SSHCommandTimeout           time.Duration
	NodeConnectTimeout          time.Duration
	TelegramPollTimeout         time.Duration
	TelegramSessionTTL          time.Duration
	CertificateRenewBefore      time.Duration
	CertificateRenewInterval    time.Duration
	CertificateIssueTimeout     time.Duration
	DNSPropagationTimeout       time.Duration
	DNSPropagationInterval      time.Duration
	MaxConcurrentDeployments    int
	MaxCertificateDistributions int
}

// Load reads configuration from environment variables and validates it.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

type lookupFunc func(string) (string, bool)

func load(lookup lookupFunc) (Config, error) {
	var validationErrors []error
	required := func(name string) string {
		value, ok := lookup(name)
		value = strings.TrimSpace(value)
		if !ok || value == "" {
			validationErrors = append(validationErrors, fmt.Errorf("%s is required", name))
		}
		return value
	}

	cfg := Config{
		TelegramBotToken:            required("TELEGRAM_BOT_TOKEN"),
		RemnawaveURL:                required("REMNAWAVE_URL"),
		RemnawaveToken:              required("REMNAWAVE_TOKEN"),
		DNSBalancerURL:              required("DNS_BALANCER_URL"),
		DNSBalancerToken:            required("DNS_BALANCER_TOKEN"),
		CloudflareAPIToken:          required("CF_API_TOKEN"),
		ACMEEmail:                   required("ACME_EMAIL"),
		DatabaseURL:                 required("DATABASE_URL"),
		DeploySSHPrivateKey:         required("DEPLOY_SSH_PRIVATE_KEY"),
		XraySNIRepoURL:              defaultXraySNIRepoURL,
		XraySNIRef:                  defaultXraySNIRef,
		HealthAddr:                  defaultHealthAddr,
		ACMEDirectoryURL:            defaultACMEDirectory,
		CertificateStorePath:        defaultCertificateStore,
		HTTPTimeout:                 30 * time.Second,
		SSHConnectTimeout:           20 * time.Second,
		SSHCommandTimeout:           5 * time.Minute,
		NodeConnectTimeout:          5 * time.Minute,
		TelegramPollTimeout:         30 * time.Second,
		TelegramSessionTTL:          15 * time.Minute,
		CertificateRenewBefore:      30 * 24 * time.Hour,
		CertificateRenewInterval:    12 * time.Hour,
		CertificateIssueTimeout:     10 * time.Minute,
		DNSPropagationTimeout:       5 * time.Minute,
		DNSPropagationInterval:      5 * time.Second,
		MaxConcurrentDeployments:    2,
		MaxCertificateDistributions: 4,
	}

	if value, ok := lookup("HEALTH_ADDR"); ok && strings.TrimSpace(value) != "" {
		cfg.HealthAddr = strings.TrimSpace(value)
	}
	if value, ok := lookup("XRAY_SNI_REPO_URL"); ok && strings.TrimSpace(value) != "" {
		cfg.XraySNIRepoURL = strings.TrimSpace(value)
	}
	if value, ok := lookup("XRAY_SNI_REF"); ok && strings.TrimSpace(value) != "" {
		cfg.XraySNIRef = strings.TrimSpace(value)
	}
	if value, ok := lookup("ACME_DIRECTORY_URL"); ok && strings.TrimSpace(value) != "" {
		cfg.ACMEDirectoryURL = strings.TrimSpace(value)
	}
	if value, ok := lookup("CERTIFICATE_STORE_PATH"); ok && strings.TrimSpace(value) != "" {
		cfg.CertificateStorePath = strings.TrimSpace(value)
	}

	durations := []struct {
		name   string
		target *time.Duration
	}{
		{"HTTP_TIMEOUT", &cfg.HTTPTimeout}, {"SSH_CONNECT_TIMEOUT", &cfg.SSHConnectTimeout},
		{"SSH_COMMAND_TIMEOUT", &cfg.SSHCommandTimeout}, {"NODE_CONNECT_TIMEOUT", &cfg.NodeConnectTimeout},
		{"TELEGRAM_POLL_TIMEOUT", &cfg.TelegramPollTimeout}, {"TELEGRAM_SESSION_TTL", &cfg.TelegramSessionTTL},
		{"CERTIFICATE_RENEW_BEFORE", &cfg.CertificateRenewBefore}, {"CERTIFICATE_RENEW_INTERVAL", &cfg.CertificateRenewInterval},
		{"CERTIFICATE_ISSUE_TIMEOUT", &cfg.CertificateIssueTimeout}, {"DNS_PROPAGATION_TIMEOUT", &cfg.DNSPropagationTimeout},
		{"DNS_PROPAGATION_INTERVAL", &cfg.DNSPropagationInterval},
	}
	for _, item := range durations {
		if value, ok := lookup(item.name); ok && strings.TrimSpace(value) != "" {
			parsed, err := time.ParseDuration(strings.TrimSpace(value))
			if err != nil || parsed <= 0 {
				validationErrors = append(validationErrors, fmt.Errorf("%s must be a positive duration", item.name))
			} else {
				*item.target = parsed
			}
		}
	}
	integers := []struct {
		name    string
		target  *int
		maximum int
	}{
		{"MAX_CONCURRENT_DEPLOYMENTS", &cfg.MaxConcurrentDeployments, 100},
		{"MAX_CERTIFICATE_DISTRIBUTIONS", &cfg.MaxCertificateDistributions, 32},
	}
	for _, item := range integers {
		if value, ok := lookup(item.name); ok && strings.TrimSpace(value) != "" {
			parsed, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || parsed <= 0 || parsed > item.maximum {
				validationErrors = append(validationErrors, fmt.Errorf("%s must be between 1 and %d", item.name, item.maximum))
			} else {
				*item.target = parsed
			}
		}
	}

	allowedUsers := required("TELEGRAM_ALLOWED_USERS")
	if allowedUsers != "" {
		cfg.TelegramAllowedUsers, validationErrors = parseAllowedUsers(allowedUsers, validationErrors)
	}

	remnaAPIIP := required("REMNA_API_IP")
	if remnaAPIIP != "" {
		cfg.RemnaAPIIP, validationErrors = parseIP("REMNA_API_IP", remnaAPIIP, validationErrors)
	}
	metricsIP := required("METRICS_IP")
	if metricsIP != "" {
		cfg.MetricsIP, validationErrors = parseIP("METRICS_IP", metricsIP, validationErrors)
	}

	for name, value := range map[string]string{
		"REMNAWAVE_URL":      cfg.RemnawaveURL,
		"DNS_BALANCER_URL":   cfg.DNSBalancerURL,
		"XRAY_SNI_REPO_URL":  cfg.XraySNIRepoURL,
		"DATABASE_URL":       cfg.DatabaseURL,
		"ACME_DIRECTORY_URL": cfg.ACMEDirectoryURL,
	} {
		if value != "" && !validURL(value, name == "DATABASE_URL") {
			validationErrors = append(validationErrors, fmt.Errorf("%s must be a valid URL", name))
		}
	}
	if address, err := mail.ParseAddress(cfg.ACMEEmail); err != nil || address.Address != cfg.ACMEEmail {
		validationErrors = append(validationErrors, errors.New("ACME_EMAIL must be a valid email address"))
	}
	if !validXraySNIRepositoryURL(cfg.XraySNIRepoURL) {
		validationErrors = append(validationErrors, errors.New("XRAY_SNI_REPO_URL must be a credential-free HTTPS URL"))
	}
	if !validPinnedRef(cfg.XraySNIRef) {
		validationErrors = append(validationErrors, errors.New("XRAY_SNI_REF must be a pinned tag or commit, not a branch HEAD"))
	}

	if len(validationErrors) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(validationErrors...))
	}
	return cfg, nil
}

func validXraySNIRepositoryURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func validPinnedRef(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, "main") || strings.EqualFold(value, "master") || strings.EqualFold(value, "HEAD") {
		return false
	}
	if strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.HasPrefix(value, "-") {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && !strings.ContainsRune("._/-", char) {
			return false
		}
	}
	return true
}

func parseAllowedUsers(value string, validationErrors []error) ([]int64, []error) {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ';' || r == ' ' })
	users := make([]int64, 0, len(parts))
	seen := make(map[int64]struct{}, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return nil, append(validationErrors, errors.New("TELEGRAM_ALLOWED_USERS must contain positive numeric IDs"))
		}
		if _, exists := seen[id]; !exists {
			users = append(users, id)
			seen[id] = struct{}{}
		}
	}
	if len(users) == 0 {
		validationErrors = append(validationErrors, errors.New("TELEGRAM_ALLOWED_USERS must contain at least one ID"))
	}
	return users, validationErrors
}

func parseIP(name, value string, validationErrors []error) (netip.Addr, []error) {
	ip, err := netip.ParseAddr(value)
	if err != nil {
		validationErrors = append(validationErrors, fmt.Errorf("%s must be a valid IP address", name))
	}
	return ip, validationErrors
}

func validURL(value string, database bool) bool {
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	if database {
		return parsed.Scheme == "postgres" || parsed.Scheme == "postgresql"
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}
