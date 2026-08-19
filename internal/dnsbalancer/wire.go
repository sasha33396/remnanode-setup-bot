package dnsbalancer

import (
	"encoding/json"
	"net/netip"
	"strings"
)

type domainWire struct {
	Domain *string     `json:"domain"`
	Zones  *[]zoneWire `json:"zones"`
}

type zoneWire struct {
	Name    *string         `json:"name"`
	TTL     *int            `json:"ttl"`
	Proxied *bool           `json:"proxied"`
	IPs     *[]string       `json:"ips"`
	Nodes   *[]zoneNodeWire `json:"nodes"`
}

type zoneNodeWire struct {
	IP      *string `json:"ip"`
	Address *string `json:"address"`
}

func (d domainWire) model() (Domain, error) {
	if d.Domain == nil || normalizeFQDN(*d.Domain) == "" || d.Zones == nil {
		return Domain{}, ErrInvalidResponse
	}
	result := Domain{Name: *d.Domain, Zones: make([]Zone, 0, len(*d.Zones))}
	for _, item := range *d.Zones {
		zone, err := item.model()
		if err != nil {
			return Domain{}, err
		}
		result.Zones = append(result.Zones, zone)
	}
	return result, nil
}

func (z zoneWire) model() (Zone, error) {
	if z.Name == nil || strings.TrimSpace(*z.Name) == "" || z.TTL == nil || *z.TTL < 1 || z.Proxied == nil || (z.IPs == nil && z.Nodes == nil) {
		return Zone{}, ErrInvalidResponse
	}
	result := Zone{Name: *z.Name, TTL: *z.TTL, Proxied: *z.Proxied}
	if z.IPs != nil {
		result.IPs = make([]string, len(*z.IPs))
		copy(result.IPs, *z.IPs)
		for _, ip := range result.IPs {
			if _, err := netip.ParseAddr(strings.TrimSpace(ip)); err != nil {
				return Zone{}, ErrInvalidResponse
			}
		}
	}
	if z.Nodes != nil {
		result.Nodes = make([]ZoneNode, 0, len(*z.Nodes))
		for _, item := range *z.Nodes {
			if item.IP == nil {
				return Zone{}, ErrInvalidResponse
			}
			if _, err := netip.ParseAddr(strings.TrimSpace(*item.IP)); err != nil {
				return Zone{}, ErrInvalidResponse
			}
			address := *item.IP
			if item.Address != nil {
				address = *item.Address
			}
			result.Nodes = append(result.Nodes, ZoneNode{IP: *item.IP, Address: address})
		}
	}
	return result, nil
}

type patchZoneRequest struct {
	IPs   []string
	Nodes []ZoneNode
}

func (r patchZoneRequest) MarshalJSON() ([]byte, error) {
	if r.Nodes != nil {
		return json.Marshal(struct {
			Nodes []ZoneNode `json:"nodes"`
		}{Nodes: r.Nodes})
	}
	return json.Marshal(struct {
		IPs []string `json:"ips"`
	}{IPs: r.IPs})
}

type statusResponse struct {
	Status *string `json:"status"`
}
