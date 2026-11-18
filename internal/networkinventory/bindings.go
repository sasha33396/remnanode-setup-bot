package networkinventory

import (
	"net/netip"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Binding contains only allowlisted network settings, never full file contents.
type Binding struct {
	ID                string        `json:"id"`
	MatchName         string        `json:"matchName,omitempty" yaml:"-"`
	MatchMAC          string        `json:"matchMAC,omitempty" yaml:"-"`
	MatchPermanentMAC string        `json:"matchPermanentMAC,omitempty" yaml:"-"`
	SetName           string        `json:"setName,omitempty" yaml:"set-name"`
	Interface         string        `json:"interface,omitempty" yaml:"-"`
	Addresses         []string      `json:"addresses,omitempty" yaml:"addresses"`
	Renderer          string        `json:"renderer,omitempty" yaml:"renderer"`
	DHCP4             bool          `json:"dhcp4" yaml:"dhcp4"`
	DHCP6             bool          `json:"dhcp6" yaml:"dhcp6"`
	Gateway4          string        `json:"gateway4,omitempty" yaml:"gateway4"`
	Gateway6          string        `json:"gateway6,omitempty" yaml:"gateway6"`
	Routes            []ConfigRoute `json:"routes,omitempty" yaml:"routes"`
	Match             struct {
		Name   string `yaml:"name"`
		MAC    string `yaml:"macaddress"`
		Driver any    `yaml:"driver"`
	} `json:"-" yaml:"match"`
}
type ConfigRoute struct {
	To     string `json:"to" yaml:"to"`
	Via    string `json:"via,omitempty" yaml:"via"`
	From   string `json:"from,omitempty" yaml:"from"`
	OnLink *bool  `json:"onLink,omitempty" yaml:"on-link"`
	Table  int    `json:"table,omitempty" yaml:"table"`
	Metric *int   `json:"metric,omitempty" yaml:"metric"`
	Scope  string `json:"scope,omitempty" yaml:"scope"`
	Type   string `json:"type,omitempty" yaml:"type"`
}

func containsIP(addresses []string, ip netip.Addr) bool {
	for _, raw := range addresses {
		if p, err := netip.ParsePrefix(raw); err == nil && p.Addr().Unmap() == ip.Unmap() {
			return true
		}
		if p, err := netip.ParseAddr(raw); err == nil && p.Unmap() == ip.Unmap() {
			return true
		}
	}
	return false
}

func netplanBindings(content string, ip netip.Addr, live map[string]string) []Binding {
	var doc struct {
		Network struct {
			Renderer  string             `yaml:"renderer"`
			Ethernets map[string]Binding `yaml:"ethernets"`
			Bridges   map[string]Binding `yaml:"bridges"`
			Bonds     map[string]Binding `yaml:"bonds"`
			VLANs     map[string]Binding `yaml:"vlans"`
		} `yaml:"network"`
	}
	if yaml.Unmarshal([]byte(content), &doc) != nil {
		return nil
	}
	var result []Binding
	for _, entries := range []map[string]Binding{doc.Network.Ethernets, doc.Network.Bridges, doc.Network.Bonds, doc.Network.VLANs} {
		for id, b := range entries {
			if !containsIP(b.Addresses, ip) {
				continue
			}
			b.ID = id
			b.MatchName = b.Match.Name
			b.MatchMAC = b.Match.MAC
			if b.Renderer == "" {
				b.Renderer = doc.Network.Renderer
			}
			// Driver matches and patterns are displayed but not guessed.
			if b.Match.Driver == nil {
				b.Interface = resolveBinding(b, live)
			}
			result = append(result, b)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func resolveBinding(b Binding, live map[string]string) string {
	var found string
	for name, mac := range live {
		if b.SetName != "" && b.SetName != name {
			continue
		}
		if b.MatchMAC != "" && !strings.EqualFold(b.MatchMAC, mac) {
			continue
		}
		// A permanent-MAC selector can only be corroborated here when the
		// observed MAC agrees; spoofed/renamed configurations remain for review.
		if b.MatchPermanentMAC != "" && !strings.EqualFold(b.MatchPermanentMAC, mac) {
			continue
		}
		// After a rename the original name need not exist; MAC confirms identity.
		if b.MatchName != "" && !(b.SetName == name && b.MatchMAC != "") && b.MatchName != name {
			continue
		}
		if b.SetName == "" && b.MatchName == "" && b.MatchMAC == "" && b.MatchPermanentMAC == "" && b.ID != name {
			continue
		}
		if found != "" {
			return ""
		}
		found = name
	}
	return found
}

func networkdBindings(content string, ip netip.Addr, live map[string]string) []Binding {
	b := Binding{}
	section := ""
	unsupported := false
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch section {
		case "[Match]":
			switch key {
			case "Name":
				b.MatchName = value
			case "MACAddress":
				b.MatchMAC = value
			case "PermanentMACAddress":
				b.MatchPermanentMAC = value
			default:
				unsupported = true
			}
		case "[Network]", "[Address]":
			if key == "Address" {
				b.Addresses = append(b.Addresses, value)
			}
		}
	}
	if !containsIP(b.Addresses, ip) {
		return nil
	}
	if !unsupported && (b.MatchName != "" || b.MatchMAC != "" || b.MatchPermanentMAC != "") {
		b.Interface = resolveBinding(b, live)
	}
	return []Binding{b}
}

// Fold a generated file only when ID, address/prefix and live interface agree
// with exactly one source. A filename alone is never sufficient evidence.
func correlateFiles(files []ConfigFile, iface string) int {
	for i := range files {
		generated := &files[i]
		if !generated.MatchesPrimary || !strings.HasPrefix(generated.Path, "/run/systemd/network/10-netplan-") || len(generated.Bindings) != 1 || generated.Bindings[0].Interface != iface || iface == "" {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path.Base(generated.Path), "10-netplan-"), ".network")
		candidates := []string{}
		for _, source := range files {
			if source.Kind != "Netplan" {
				continue
			}
			for _, b := range source.Bindings {
				if b.ID != id || b.Interface != iface || (b.Renderer != "" && b.Renderer != "networkd") {
					continue
				}
				// All source addresses must be present in the generated configuration.
				agrees := len(b.Addresses) > 0
				for _, address := range b.Addresses {
					found := false
					for _, other := range generated.Bindings[0].Addresses {
						if address == other {
							found = true
						}
					}
					agrees = agrees && found
				}
				if agrees {
					candidates = append(candidates, source.Path)
				}
			}
		}
		if len(candidates) == 1 {
			generated.GeneratedFrom = candidates[0]
		}
	}
	count := 0
	for _, file := range files {
		if !file.MatchesPrimary || file.GeneratedFrom != "" {
			continue
		}
		if file.Kind == "Netplan" {
			for _, b := range file.Bindings {
				if b.Interface == iface && iface != "" {
					count++
				} else {
					count += 2
				}
			}
			if len(file.Bindings) == 0 {
				count += 2
			}
		} else {
			if file.Kind == "networkd" && (strings.HasPrefix(file.Path, "/run/systemd/network/10-netplan-") || len(file.Bindings) != 1 || file.Bindings[0].Interface != iface || iface == "") {
				count += 2
			} else {
				count++
			}
		}
	}
	return count
}
