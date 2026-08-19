package telegram

import (
	"net/netip"
	"strings"
	"unicode"
	"unicode/utf8"
)

func validNodeName(value string) bool {
	value = strings.TrimSpace(value)
	length := utf8.RuneCountInString(value)
	if length < 3 || length > 30 {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

var nonPublicPrefixes = mustPrefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
	"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
	"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"224.0.0.0/4", "240.0.0.0/4", "::/128", "::1/128", "2001:db8::/32",
	"fc00::/7", "fe80::/10", "ff00::/8",
)

func parsePublicIP(value string) (netip.Addr, bool) {
	address, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil || address.Zone() != "" {
		return netip.Addr{}, false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() {
		return netip.Addr{}, false
	}
	for _, prefix := range nonPublicPrefixes {
		if prefix.Contains(address) {
			return netip.Addr{}, false
		}
	}
	return address, true
}

func parsePublicIPv4(value string) (netip.Addr, bool) {
	address, valid := parsePublicIP(value)
	return address, valid && address.Is4()
}

func mustPrefixes(values ...string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		result = append(result, netip.MustParsePrefix(value))
	}
	return result
}
