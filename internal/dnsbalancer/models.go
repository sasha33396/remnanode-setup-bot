// Package dnsbalancer provides a typed client for the
// remnawave-cloudflare-nodes HTTP API.
package dnsbalancer

// Domain is one configured parent domain and its actual zones.
type Domain struct {
	Name  string
	Zones []Zone
}

// Zone is a DNS-balancer zone. IPs and Nodes are separate formats supported by
// the API and must not be conflated when updating one of them.
type Zone struct {
	Name    string
	TTL     int
	Proxied bool
	IPs     []string
	Nodes   []ZoneNode
}

// ZoneNode is the advanced zone entry format.
type ZoneNode struct {
	IP      string `json:"ip"`
	Address string `json:"address"`
}

// ZoneMatch identifies an exact FQDN using the domain/zone values returned by
// the API. Domain and ZoneName retain their original values for the PATCH path.
type ZoneMatch struct {
	Domain   string
	ZoneName string
	FQDN     string
	Zone     Zone
}

// AddIPResult reports the complete simple IP list after AddIP.
type AddIPResult struct {
	FQDN  string
	Added bool
	IPs   []string
}

// ReplaceIPResult reports an idempotent simple or advanced IP replacement.
type ReplaceIPResult struct {
	FQDN    string
	Changed bool
	IPs     []string
	Nodes   []ZoneNode
}
