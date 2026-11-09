package abuse

import (
	"net/netip"
	"sort"
	"time"
)

type Risk string

const (
	RiskLow      Risk = "LOW"
	RiskMedium   Risk = "MEDIUM"
	RiskHigh     Risk = "HIGH"
	RiskCritical Risk = "CRITICAL"
)

type Thresholds struct {
	SMTP25Warn10M    int
	SMTP25High10M    int
	SubmissionWarn1H int
	SubmissionHigh1H int
	UniqueIPWarn1H   int
	UniqueIPHigh24H  int
	UniqueASNWarn24H int
}

func DefaultThresholds() Thresholds {
	return Thresholds{SMTP25Warn10M: 10, SMTP25High10M: 50, SubmissionWarn1H: 100, SubmissionHigh1H: 300, UniqueIPWarn1H: 10, UniqueIPHigh24H: 25, UniqueASNWarn24H: 5}
}

type Intelligence struct {
	ASN      uint32
	Provider string
	Category string
}

type Assessment struct {
	PanelID, UserID                          string
	Risk                                     Risk
	Score                                    int
	Reasons                                  []string
	SMTP25Attempts10M, SubmissionAttempts1H  int
	UniqueIPs1H, UniqueIPs24H, UniqueASNs24H int
	UniqueMailDestinations1H                 int
	IPsFromAPI                               bool
	FirstSeen, LastSeen                      time.Time
}

// Assess always derives mail-port activity from node access.log events. When
// observedIPs is non-nil, unique IP/ASN windows come from Remnawave
// IP-control's lastSeen values; nil preserves the log-based fallback.
func Assess(panelID, userID string, events []Event, intel map[netip.Addr]Intelligence, observedIPs map[netip.Addr]time.Time, now time.Time, thresholds Thresholds) Assessment {
	result := Assessment{PanelID: panelID, UserID: userID, Risk: RiskLow}
	ips1h, ips24h, asns24h := map[netip.Addr]struct{}{}, map[netip.Addr]struct{}{}, map[uint32]struct{}{}
	logIPs := map[netip.Addr]time.Time{}
	mailDestinations1h := map[string]struct{}{}
	hosting := false
	for _, event := range events {
		if event.PanelID != panelID || event.UserID != userID || event.OccurredAt.Before(now.Add(-24*time.Hour)) || event.OccurredAt.After(now.Add(time.Minute)) {
			continue
		}
		if result.FirstSeen.IsZero() || event.OccurredAt.Before(result.FirstSeen) {
			result.FirstSeen = event.OccurredAt
		}
		if event.OccurredAt.After(result.LastSeen) {
			result.LastSeen = event.OccurredAt
		}
		if event.OccurredAt.After(logIPs[event.ClientIP]) {
			logIPs[event.ClientIP] = event.OccurredAt
		}
		if event.OccurredAt.After(now.Add(-time.Hour)) {
			if event.DestinationPort == 465 || event.DestinationPort == 587 {
				result.SubmissionAttempts1H++
				mailDestinations1h[event.Destination] = struct{}{}
			}
		}
		if event.OccurredAt.After(now.Add(-10*time.Minute)) && event.DestinationPort == 25 {
			result.SMTP25Attempts10M++
		}
	}
	if observedIPs == nil {
		observedIPs = logIPs
	} else {
		result.IPsFromAPI = true
	}
	for ip, lastSeen := range observedIPs {
		if lastSeen.Before(now.Add(-24*time.Hour)) || lastSeen.After(now.Add(time.Minute)) {
			continue
		}
		ips24h[ip] = struct{}{}
		if lastSeen.After(now.Add(-time.Hour)) {
			ips1h[ip] = struct{}{}
		}
		info := intel[ip]
		if info.ASN != 0 {
			asns24h[info.ASN] = struct{}{}
		}
		if info.Category == "HOSTING" || info.Category == "PROXY" {
			hosting = true
		}
	}
	result.UniqueIPs1H, result.UniqueIPs24H, result.UniqueASNs24H = len(ips1h), len(ips24h), len(asns24h)
	result.UniqueMailDestinations1H = len(mailDestinations1h)
	if result.SMTP25Attempts10M >= thresholds.SMTP25High10M {
		result.Score += 70
		result.Reasons = append(result.Reasons, "mass SMTP/25 attempts")
	} else if result.SMTP25Attempts10M >= thresholds.SMTP25Warn10M {
		result.Score += 40
		result.Reasons = append(result.Reasons, "elevated SMTP/25 attempts")
	}
	if result.SubmissionAttempts1H >= thresholds.SubmissionHigh1H && result.UniqueMailDestinations1H >= 3 {
		result.Score += 65
		result.Reasons = append(result.Reasons, "mass SMTP submission attempts")
	} else if result.SubmissionAttempts1H >= thresholds.SubmissionWarn1H && result.UniqueMailDestinations1H >= 3 {
		result.Score += 35
		result.Reasons = append(result.Reasons, "elevated SMTP submission attempts")
	}
	if result.UniqueIPs24H >= thresholds.UniqueIPHigh24H {
		result.Score += 35
		result.Reasons = append(result.Reasons, "many client IPs in 24h")
	} else if result.UniqueIPs1H >= thresholds.UniqueIPWarn1H {
		result.Score += 20
		result.Reasons = append(result.Reasons, "many client IPs in 1h")
	}
	if result.UniqueASNs24H >= thresholds.UniqueASNWarn24H {
		result.Score += 20
		result.Reasons = append(result.Reasons, "many client ASNs in 24h")
	}
	if hosting {
		result.Score += 15
		result.Reasons = append(result.Reasons, "hosting or proxy client network")
	}
	sort.Strings(result.Reasons)
	switch {
	case result.Score >= 100:
		result.Risk = RiskCritical
	case result.Score >= 50:
		result.Risk = RiskHigh
	case result.Score >= 20:
		result.Risk = RiskMedium
	}
	return result
}
