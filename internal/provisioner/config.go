package provisioner

import (
	"errors"
	"net/netip"
	"time"
)

// Config contains non-persistent inputs for one provisioning run. The
// Remnanode secret is private so formatting Config cannot reveal it.
type Config struct {
	RemnawaveAPIIP      netip.Addr
	MetricsIP           netip.Addr
	Timezone            string
	NodeExporterVersion string
	CommandTimeout      time.Duration
	remnanodeSecret     []byte
}

func NewConfig(remnawaveAPIIP, metricsIP netip.Addr, remnanodeSecret []byte) (Config, error) {
	if !remnawaveAPIIP.IsValid() || !metricsIP.IsValid() || len(remnanodeSecret) == 0 {
		return Config{}, errors.New("invalid provisioner configuration")
	}
	config := Config{
		RemnawaveAPIIP:      remnawaveAPIIP,
		MetricsIP:           metricsIP,
		Timezone:            "Europe/Moscow",
		NodeExporterVersion: "1.8.2",
		CommandTimeout:      5 * time.Minute,
		remnanodeSecret:     append([]byte(nil), remnanodeSecret...),
	}
	return config, nil
}

// Destroy removes the retained in-memory copy after the workflow completes.
func (c *Config) Destroy() {
	for index := range c.remnanodeSecret {
		c.remnanodeSecret[index] = 0
	}
	c.remnanodeSecret = nil
}

func (c Config) normalized() (Config, error) {
	if !c.RemnawaveAPIIP.IsValid() || !c.MetricsIP.IsValid() || len(c.remnanodeSecret) == 0 {
		return Config{}, errors.New("invalid provisioner configuration")
	}
	if c.Timezone == "" {
		c.Timezone = "Europe/Moscow"
	}
	if c.NodeExporterVersion == "" {
		c.NodeExporterVersion = "1.8.2"
	}
	if c.CommandTimeout <= 0 {
		c.CommandTimeout = 5 * time.Minute
	}
	return c, nil
}
