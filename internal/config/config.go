// Package config loads and validates application configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type DNSMode string

const (
	DNSModeEnabled  DNSMode = "enabled"
	DNSModeDisabled DNSMode = "disabled"
)

// PanelConfig owns one isolated Remnawave integration and its optional DNS
// and certificate issuer credentials. Callers must never log this value.
type PanelConfig struct {
	ID                 string
	Name               string
	RemnawaveURL       string
	RemnawaveToken     string
	RemnawaveAPIIP     netip.Addr
	DNSMode            DNSMode
	DNSBalancerURL     string
	DNSBalancerToken   string
	CloudflareAPIToken string
}

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
	Panels                      []PanelConfig
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
	NodeMonitorInterval         time.Duration
	CertificateRenewBefore      time.Duration
	CertificateRenewInterval    time.Duration
	CertificateIssueTimeout     time.Duration
	DNSPropagationTimeout       time.Duration
	DNSPropagationInterval      time.Duration
	MaxConcurrentDeployments    int
	MaxCertificateDistributions int
	NodeMonitorConfirmations    int
	NodeCriticalOnlineFloor     int
	NodeCriticalOnlineRatio     int
	NodeCriticalOnlineCap       int
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

	panelsFile, hasPanelsFile := lookup("PANELS_CONFIG_FILE")
	panelsFile = strings.TrimSpace(panelsFile)
	hasPanelsFile = hasPanelsFile && panelsFile != ""
	panelsJSON, hasPanelsJSON := lookup("PANELS_JSON")
	hasPanelsJSON = hasPanelsJSON && strings.TrimSpace(panelsJSON) != ""
	if hasPanelsFile && hasPanelsJSON {
		validationErrors = append(validationErrors, errors.New("PANELS_CONFIG_FILE and PANELS_JSON cannot be used together"))
	}
	hasPanelConfig := hasPanelsFile || hasPanelsJSON
	legacyRequired := func(name string) string {
		if hasPanelConfig {
			value, _ := lookup(name)
			return strings.TrimSpace(value)
		}
		return required(name)
	}
	cfg := Config{
		TelegramBotToken:            required("TELEGRAM_BOT_TOKEN"),
		RemnawaveURL:                legacyRequired("REMNAWAVE_URL"),
		RemnawaveToken:              legacyRequired("REMNAWAVE_TOKEN"),
		DNSBalancerURL:              legacyRequired("DNS_BALANCER_URL"),
		DNSBalancerToken:            legacyRequired("DNS_BALANCER_TOKEN"),
		CloudflareAPIToken:          legacyRequired("CF_API_TOKEN"),
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
		NodeMonitorInterval:         2 * time.Minute,
		CertificateRenewBefore:      30 * 24 * time.Hour,
		CertificateRenewInterval:    12 * time.Hour,
		CertificateIssueTimeout:     10 * time.Minute,
		DNSPropagationTimeout:       5 * time.Minute,
		DNSPropagationInterval:      5 * time.Second,
		MaxConcurrentDeployments:    2,
		MaxCertificateDistributions: 4,
		NodeMonitorConfirmations:    2,
		NodeCriticalOnlineFloor:     80,
		NodeCriticalOnlineRatio:     40,
		NodeCriticalOnlineCap:       200,
	}
	remnaAPIIP := legacyRequired("REMNA_API_IP")
	if remnaAPIIP != "" {
		cfg.RemnaAPIIP, validationErrors = parseIP("REMNA_API_IP", remnaAPIIP, validationErrors)
	}
	if hasPanelsFile {
		cfg.Panels, validationErrors = parsePanelsFile(panelsFile, lookup, cfg.CloudflareAPIToken, validationErrors)
	} else if hasPanelsJSON {
		cfg.Panels, validationErrors = parsePanels(panelsJSON, lookup, cfg.CloudflareAPIToken, validationErrors)
	} else {
		cfg.Panels = []PanelConfig{{ID: "default", Name: "Default", RemnawaveURL: cfg.RemnawaveURL, RemnawaveToken: cfg.RemnawaveToken, RemnawaveAPIIP: cfg.RemnaAPIIP, DNSMode: DNSModeEnabled, DNSBalancerURL: cfg.DNSBalancerURL, DNSBalancerToken: cfg.DNSBalancerToken, CloudflareAPIToken: cfg.CloudflareAPIToken}}
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
		{"NODE_MONITOR_INTERVAL", &cfg.NodeMonitorInterval},
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
		{"NODE_MONITOR_CONFIRMATIONS", &cfg.NodeMonitorConfirmations, 10},
		{"NODE_CRITICAL_ONLINE_FLOOR", &cfg.NodeCriticalOnlineFloor, 100000},
		{"NODE_CRITICAL_ONLINE_RATIO", &cfg.NodeCriticalOnlineRatio, 100},
		{"NODE_CRITICAL_ONLINE_CAP", &cfg.NodeCriticalOnlineCap, 100000},
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
	if cfg.NodeCriticalOnlineCap < cfg.NodeCriticalOnlineFloor {
		validationErrors = append(validationErrors, errors.New("NODE_CRITICAL_ONLINE_CAP must be greater than or equal to NODE_CRITICAL_ONLINE_FLOOR"))
	}

	allowedUsers := required("TELEGRAM_ALLOWED_USERS")
	if allowedUsers != "" {
		cfg.TelegramAllowedUsers, validationErrors = parseAllowedUsers(allowedUsers, validationErrors)
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
	for index, panel := range cfg.Panels {
		if !validURL(panel.RemnawaveURL, false) {
			validationErrors = append(validationErrors, fmt.Errorf("panel %d Remnawave URL must be valid", index))
		}
		if panel.DNSMode == DNSModeEnabled && !validURL(panel.DNSBalancerURL, false) {
			validationErrors = append(validationErrors, fmt.Errorf("panel %s DNS-balancer URL must be valid", panel.ID))
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

type panelJSON struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	RemnawaveURL       string  `json:"remnawave_url"`
	RemnawaveTokenEnv  string  `json:"remnawave_token_env"`
	RemnawaveAPIIP     string  `json:"remnawave_api_ip"`
	CloudflareTokenEnv string  `json:"cloudflare_token_env"`
	DNS                dnsJSON `json:"dns"`
}

type dnsJSON struct {
	Mode     DNSMode `json:"mode"`
	URL      string  `json:"url"`
	TokenEnv string  `json:"token_env"`
}

type panelsYAML struct {
	Panels []panelYAML `yaml:"panels"`
}

type panelYAML struct {
	ID          string          `yaml:"id"`
	Name        string          `yaml:"name"`
	Remnawave   remnawaveYAML   `yaml:"remnawave"`
	DNS         dnsYAML         `yaml:"dns"`
	Certificate certificateYAML `yaml:"certificate"`
}

type remnawaveYAML struct {
	URL      string `yaml:"url"`
	TokenEnv string `yaml:"token_env"`
	APIIP    string `yaml:"api_ip"`
}

type dnsYAML struct {
	Mode     DNSMode `yaml:"mode"`
	URL      string  `yaml:"url"`
	TokenEnv string  `yaml:"token_env"`
}

type certificateYAML struct {
	CloudflareTokenEnv string `yaml:"cloudflare_token_env"`
}

func parsePanelsFile(path string, lookup lookupFunc, fallbackCloudflare string, validationErrors []error) ([]PanelConfig, []error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, append(validationErrors, errors.New("PANELS_CONFIG_FILE could not be opened"))
	}
	defer file.Close()
	if info, statErr := file.Stat(); statErr != nil || info.Size() > 1<<20 {
		return nil, append(validationErrors, errors.New("PANELS_CONFIG_FILE could not be read safely"))
	}

	var document panelsYAML
	decoder := yaml.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil || len(document.Panels) == 0 {
		return nil, append(validationErrors, errors.New("PANELS_CONFIG_FILE must contain a non-empty valid panels list"))
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, append(validationErrors, errors.New("PANELS_CONFIG_FILE must contain exactly one YAML document"))
	}
	raw := make([]panelJSON, 0, len(document.Panels))
	for _, item := range document.Panels {
		raw = append(raw, panelJSON{
			ID:                 item.ID,
			Name:               item.Name,
			RemnawaveURL:       item.Remnawave.URL,
			RemnawaveTokenEnv:  item.Remnawave.TokenEnv,
			RemnawaveAPIIP:     item.Remnawave.APIIP,
			CloudflareTokenEnv: item.Certificate.CloudflareTokenEnv,
			DNS: dnsJSON{
				Mode:     item.DNS.Mode,
				URL:      item.DNS.URL,
				TokenEnv: item.DNS.TokenEnv,
			},
		})
	}
	return validatePanels(raw, lookup, fallbackCloudflare, validationErrors)
}

func parsePanels(value string, lookup lookupFunc, fallbackCloudflare string, validationErrors []error) ([]PanelConfig, []error) {
	var raw []panelJSON
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&raw); err != nil || len(raw) == 0 {
		return nil, append(validationErrors, errors.New("PANELS_JSON must be a non-empty valid JSON array"))
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, append(validationErrors, errors.New("PANELS_JSON must contain exactly one JSON array"))
	}
	return validatePanels(raw, lookup, fallbackCloudflare, validationErrors)
}

func validatePanels(raw []panelJSON, lookup lookupFunc, fallbackCloudflare string, validationErrors []error) ([]PanelConfig, []error) {
	if len(raw) > 32 {
		return nil, append(validationErrors, errors.New("panel configuration cannot contain more than 32 panels"))
	}
	seen := make(map[string]struct{}, len(raw))
	result := make([]PanelConfig, 0, len(raw))
	for index, item := range raw {
		item.ID, item.Name = strings.TrimSpace(item.ID), strings.TrimSpace(item.Name)
		if !validPanelID(item.ID) {
			validationErrors = append(validationErrors, fmt.Errorf("panel %d has an invalid id", index))
		}
		if _, exists := seen[item.ID]; exists {
			validationErrors = append(validationErrors, fmt.Errorf("panel id %q is duplicated", item.ID))
		}
		seen[item.ID] = struct{}{}
		if item.Name == "" || len(item.Name) > 80 {
			validationErrors = append(validationErrors, fmt.Errorf("panel %s has an invalid name", item.ID))
		}
		remnawaveToken := referencedSecret(item.RemnawaveTokenEnv, lookup)
		if remnawaveToken == "" {
			validationErrors = append(validationErrors, fmt.Errorf("panel %s Remnawave token environment variable is missing", item.ID))
		}
		remnawaveAPIIP, err := netip.ParseAddr(strings.TrimSpace(item.RemnawaveAPIIP))
		if err != nil {
			validationErrors = append(validationErrors, fmt.Errorf("panel %s Remnawave API IP must be a valid IP address", item.ID))
		}
		mode := item.DNS.Mode
		if mode == "" {
			mode = DNSModeDisabled
		}
		if mode != DNSModeEnabled && mode != DNSModeDisabled {
			validationErrors = append(validationErrors, fmt.Errorf("panel %s DNS mode must be enabled or disabled", item.ID))
		}
		dnsToken := ""
		if mode == DNSModeEnabled {
			dnsToken = referencedSecret(item.DNS.TokenEnv, lookup)
			if strings.TrimSpace(item.DNS.URL) == "" || dnsToken == "" {
				validationErrors = append(validationErrors, fmt.Errorf("panel %s enabled DNS configuration is incomplete", item.ID))
			}
		}
		cloudflareToken := fallbackCloudflare
		if strings.TrimSpace(item.CloudflareTokenEnv) != "" {
			cloudflareToken = referencedSecret(item.CloudflareTokenEnv, lookup)
		}
		if cloudflareToken == "" {
			validationErrors = append(validationErrors, fmt.Errorf("panel %s Cloudflare token environment variable is missing", item.ID))
		}
		result = append(result, PanelConfig{ID: item.ID, Name: item.Name, RemnawaveURL: strings.TrimSpace(item.RemnawaveURL), RemnawaveToken: remnawaveToken, RemnawaveAPIIP: remnawaveAPIIP, DNSMode: mode, DNSBalancerURL: strings.TrimSpace(item.DNS.URL), DNSBalancerToken: dnsToken, CloudflareAPIToken: cloudflareToken})
	}
	return result, validationErrors
}

func referencedSecret(name string, lookup lookupFunc) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	value, _ := lookup(name)
	return strings.TrimSpace(value)
}

func validPanelID(value string) bool {
	if len(value) < 1 || len(value) > 32 {
		return false
	}
	for index, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || (index > 0 && (char == '-' || char == '_')) {
			continue
		}
		return false
	}
	return true
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
