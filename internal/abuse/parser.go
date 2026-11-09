package abuse

import (
	"errors"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var accessLine = regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)?) from (\S+) accepted (\w+):(\S+) \[([^]]+)\] email: (\S+)\s*$`)

type Event struct {
	PanelID         string
	NodeUUID        string
	NodeName        string
	NodeIP          netip.Addr
	OccurredAt      time.Time
	ClientIP        netip.Addr
	ClientPort      uint16
	DestinationIP   netip.Addr
	Destination     string
	DestinationPort uint16
	Network         string
	Inbound         string
	Outbound        string
	UserID          string
	Accepted        bool
	Blocked         bool
	ASN             uint32
	Provider        string
	Category        string
}

func ParseAccessLine(line string, zone *time.Location) (Event, error) {
	match := accessLine.FindStringSubmatch(strings.TrimSpace(line))
	if match == nil {
		return Event{}, errors.New("unsupported access log line")
	}
	if zone == nil {
		zone = time.UTC
	}
	occurredAt, err := time.ParseInLocation("2006/01/02 15:04:05.999999999", match[1], zone)
	if err != nil {
		return Event{}, errors.New("invalid access log timestamp")
	}
	clientIP, clientPort, err := parseEndpoint(match[2])
	if err != nil || !clientIP.IsValid() {
		return Event{}, errors.New("invalid access log client endpoint")
	}
	destinationIP, destination, destinationPort, err := parseDestination(match[4])
	if err != nil {
		return Event{}, errors.New("invalid access log destination")
	}
	inbound, outbound := parseRoute(match[5])
	if inbound == "" || outbound == "" {
		return Event{}, errors.New("invalid access log route")
	}
	return Event{
		OccurredAt: occurredAt.UTC(), ClientIP: clientIP.Unmap(), ClientPort: clientPort,
		DestinationIP: destinationIP.Unmap(), Destination: destination, DestinationPort: destinationPort,
		Network: strings.ToLower(match[3]), Inbound: inbound, Outbound: outbound,
		UserID: strings.TrimSpace(match[6]), Accepted: true, Blocked: strings.EqualFold(outbound, "BLOCK"),
	}, nil
}

func parseEndpoint(value string) (netip.Addr, uint16, error) {
	host, rawPort, err := net.SplitHostPort(value)
	if err != nil {
		index := strings.LastIndexByte(value, ':')
		if index <= 0 {
			return netip.Addr{}, 0, err
		}
		host, rawPort = value[:index], value[index+1:]
	}
	host = strings.Trim(host, "[]")
	address, addressErr := netip.ParseAddr(host)
	port, portErr := strconv.ParseUint(rawPort, 10, 16)
	if addressErr != nil || portErr != nil || port == 0 {
		return netip.Addr{}, 0, errors.New("invalid endpoint")
	}
	return address, uint16(port), nil
}

func parseDestination(value string) (netip.Addr, string, uint16, error) {
	index := strings.LastIndexByte(value, ':')
	if index <= 0 {
		return netip.Addr{}, "", 0, errors.New("destination port is absent")
	}
	host := strings.Trim(value[:index], "[]")
	port, err := strconv.ParseUint(value[index+1:], 10, 16)
	if err != nil || port == 0 {
		return netip.Addr{}, "", 0, errors.New("invalid destination port")
	}
	address, _ := netip.ParseAddr(host)
	return address, strings.ToLower(host), uint16(port), nil
}

func parseRoute(value string) (string, string) {
	normalized := strings.ReplaceAll(value, "->", ">>")
	parts := strings.Split(normalized, ">>")
	if len(parts) < 2 {
		return "", ""
	}
	return strings.TrimSpace(parts[0]), strings.ToUpper(strings.TrimSpace(parts[len(parts)-1]))
}
