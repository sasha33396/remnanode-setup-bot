package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"remnanode-setup-bot/internal/abuse"
	"remnanode-setup-bot/internal/remnawave"
	sshclient "remnanode-setup-bot/internal/ssh"
	"remnanode-setup-bot/internal/telegram"
)

const abuseChunkSize int64 = 512 * 1024

type AbuseMonitorConfig struct {
	Interval, Cooldown, Retention time.Duration
	LogPath                       string
	LogLocation                   *time.Location
	MaxConcurrent                 int
	Thresholds                    abuse.Thresholds
	Intelligence                  *abuse.LocalIntelligence
	IPInfo                        *abuse.IPInfoClient
	ASNOverrides                  map[uint32]string
}

type AbuseMonitor struct {
	app               *TelegramApplication
	store             abuse.Store
	ssh               *sshclient.Client
	messenger         telegram.Messenger
	allowed           []int64
	config            AbuseMonitorConfig
	logger            *slog.Logger
	apiMu             sync.RWMutex
	apiIPs            map[[2]string]map[netip.Addr]time.Time
	apiPanels         map[string]bool
	ipInfoLookups     atomic.Int64
	ipInfoErrorLogged atomic.Bool
}

func NewAbuseMonitor(app *TelegramApplication, store abuse.Store, ssh *sshclient.Client, messenger telegram.Messenger, allowed []int64, config AbuseMonitorConfig, logger *slog.Logger) (*AbuseMonitor, error) {
	if app == nil || store == nil || ssh == nil || messenger == nil || len(allowed) == 0 || config.Interval <= 0 || config.Cooldown <= 0 || config.Retention < 24*time.Hour || config.MaxConcurrent <= 0 || !filepath.IsAbs(config.LogPath) || strings.ContainsAny(config.LogPath, "\r\n\x00") {
		return nil, errors.New("invalid abuse monitor configuration")
	}
	if config.LogLocation == nil {
		config.LogLocation = time.UTC
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &AbuseMonitor{app: app, store: store, ssh: ssh, messenger: messenger, allowed: append([]int64(nil), allowed...), config: config, logger: logger, apiIPs: make(map[[2]string]map[netip.Addr]time.Time), apiPanels: make(map[string]bool)}, nil
}

func (m *AbuseMonitor) Run(ctx context.Context) error {
	m.poll(ctx)
	ticker := time.NewTicker(m.config.Interval)
	defer ticker.Stop()
	cleanup := time.NewTicker(6 * time.Hour)
	defer cleanup.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.poll(ctx)
		case <-cleanup.C:
			if err := m.store.Cleanup(ctx, time.Now().Add(-m.config.Retention), time.Now()); err != nil {
				m.logger.Warn("abuse monitor cleanup failed", slog.Any("error", err))
			}
		}
	}
}

func (m *AbuseMonitor) poll(ctx context.Context) {
	m.ipInfoLookups.Store(0)
	m.ipInfoErrorLogged.Store(false)
	targets, err := m.targets(ctx)
	if err != nil {
		m.logger.Warn("abuse monitor inventory failed", slog.Any("error", err))
		return
	}
	sem := make(chan struct{}, m.config.MaxConcurrent)
	var workers sync.WaitGroup
	var snapshotMu sync.Mutex
	snapshots := make(map[[2]string]map[netip.Addr]time.Time)
	expected, succeeded := make(map[string]int), make(map[string]int)
	for _, target := range targets {
		expected[target.panelID]++
	}
	for _, target := range targets {
		target := target
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if err := m.collect(ctx, target); err != nil {
				m.logger.Warn("abuse log collection skipped node", slog.String("panel_id", target.panelID), slog.String("node_name", target.nodeName), slog.Any("error", err))
			}
			users, apiErr := m.fetchNodeUserIPs(ctx, target)
			if apiErr != nil {
				m.logger.Warn("Remnawave IP-control snapshot skipped node", slog.String("panel_id", target.panelID), slog.String("node_name", target.nodeName), slog.Any("error", apiErr))
				return
			}
			snapshotMu.Lock()
			succeeded[target.panelID]++
			for _, user := range users {
				key := [2]string{target.panelID, user.UserID}
				if snapshots[key] == nil {
					snapshots[key] = make(map[netip.Addr]time.Time)
				}
				for _, item := range user.IPs {
					if item.LastSeen.After(snapshots[key][item.IP]) {
						snapshots[key][item.IP] = item.LastSeen
					}
				}
			}
			snapshotMu.Unlock()
		}()
	}
	workers.Wait()
	completePanels := make(map[string]bool)
	for panelID, count := range expected {
		completePanels[panelID] = count > 0 && succeeded[panelID] == count
	}
	m.apiMu.Lock()
	m.apiIPs, m.apiPanels = snapshots, completePanels
	m.apiMu.Unlock()
	m.analyze(ctx)
}

func (m *AbuseMonitor) fetchNodeUserIPs(ctx context.Context, target managedActualizeTarget) ([]remnawave.NodeUserIPs, error) {
	panel := m.app.panels[target.panelID]
	provider, ok := panel.Service.remnawave.(remnawave.NodeUserIPsAPI)
	if !ok {
		return nil, errors.New("Remnawave IP-control is unavailable")
	}
	return provider.FetchNodeUserIPs(ctx, target.nodeUUID)
}

func (m *AbuseMonitor) targets(ctx context.Context) ([]managedActualizeTarget, error) {
	var targets []managedActualizeTarget
	for _, panelID := range m.app.order {
		panel := m.app.panels[panelID]
		nodes, err := panel.Service.remnawave.GetNodes(ctx)
		if err != nil {
			return nil, errors.New("load nodes for abuse monitor failed")
		}
		for _, node := range nodes {
			deployment, findErr := m.app.repository.FindDeploymentByPanelNodeUUID(ctx, panelID, node.UUID)
			if findErr != nil {
				continue
			}
			address, parseErr := netip.ParseAddr(strings.TrimSpace(node.Address))
			if parseErr != nil || !address.IsGlobalUnicast() {
				continue
			}
			targets = append(targets, managedActualizeTarget{panelID: panelID, panelName: panel.Name, nodeUUID: node.UUID, nodeName: node.Name, deploymentID: deployment.ID, address: address.Unmap()})
		}
	}
	return targets, nil
}

func (m *AbuseMonitor) collect(ctx context.Context, target managedActualizeTarget) error {
	connection, err := m.ssh.ConnectWithDeploymentKey(ctx, target.deploymentID, target.address, "root")
	if err != nil {
		return errors.New("SSH connection failed")
	}
	defer connection.Close()
	quoted := shellQuote(m.config.LogPath)
	stat, err := connection.Run(ctx, sshclient.CommandRequest{Command: "set -eu; stat -c '%i %s' -- " + quoted, Timeout: 30 * time.Second})
	if err != nil {
		return errors.New("access log stat failed")
	}
	fields := strings.Fields(stat.Stdout)
	if len(fields) != 2 {
		return errors.New("invalid access log stat")
	}
	inode, _ := strconv.ParseInt(fields[0], 10, 64)
	size, _ := strconv.ParseInt(fields[1], 10, 64)
	if inode <= 0 || size < 0 {
		return errors.New("invalid access log metadata")
	}
	checkpoint, err := m.store.LoadCheckpoint(ctx, target.panelID, target.nodeUUID)
	if err != nil {
		return err
	}
	if checkpoint.Inode == 0 {
		checkpoint.Inode, checkpoint.Offset = inode, size
		checkpoint.PanelID, checkpoint.NodeUUID = target.panelID, target.nodeUUID
		_, err = m.store.SaveEvents(ctx, checkpoint, nil)
		return err
	}
	var chunks []string
	if checkpoint.Inode != inode {
		rotated := m.config.LogPath + ".1"
		rotatedQuoted := shellQuote(rotated)
		metadataCommand := fmt.Sprintf("set -eu; if [ -f %s ] && [ \"$(stat -c %%i -- %s)\" = %s ]; then printf 'plain '; stat -c %%s -- %s; elif [ -f %s.gz ]; then printf 'gzip '; gzip -cd -- %s.gz | wc -c; else printf 'none 0'; fi", rotatedQuoted, rotatedQuoted, strconv.FormatInt(checkpoint.Inode, 10), rotatedQuoted, rotatedQuoted, rotatedQuoted)
		metadata, metadataErr := connection.Run(ctx, sshclient.CommandRequest{Command: metadataCommand, Timeout: 30 * time.Second})
		rotatedFields := strings.Fields(metadata.Stdout)
		if metadataErr == nil && len(rotatedFields) == 2 && rotatedFields[0] != "none" {
			rotatedSize, parseErr := strconv.ParseInt(rotatedFields[1], 10, 64)
			if parseErr == nil && rotatedSize > checkpoint.Offset {
				count := rotatedSize - checkpoint.Offset
				if count > abuseChunkSize {
					count = abuseChunkSize
				}
				var readCommand string
				if rotatedFields[0] == "plain" {
					readCommand = fmt.Sprintf("dd if=%s bs=1 skip=%d count=%d status=none", rotatedQuoted, checkpoint.Offset, count)
				} else {
					readCommand = fmt.Sprintf("gzip -cd -- %s.gz | tail -c +%d | head -c %d", rotatedQuoted, checkpoint.Offset+1, count)
				}
				result, runErr := connection.Run(ctx, sshclient.CommandRequest{Command: readCommand, Timeout: 30 * time.Second})
				if runErr != nil {
					return errors.New("rotated access log read failed")
				}
				chunks = append(chunks, result.Stdout)
				checkpoint.Offset += int64(len(result.Stdout))
				if checkpoint.Offset < rotatedSize {
					return m.saveLogChunks(ctx, target, checkpoint, chunks...)
				}
			}
		}
		checkpoint.Offset = 0
		checkpoint.Inode = inode
	} else if size < checkpoint.Offset {
		checkpoint.Offset = 0
	}
	remaining := size - checkpoint.Offset
	if remaining > abuseChunkSize {
		remaining = abuseChunkSize
	}
	if remaining > 0 {
		command := fmt.Sprintf("dd if=%s bs=1 skip=%d count=%d status=none", quoted, checkpoint.Offset, remaining)
		result, runErr := connection.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: 30 * time.Second})
		if runErr != nil {
			return errors.New("access log incremental read failed")
		}
		chunks = append(chunks, result.Stdout)
		checkpoint.Offset += int64(len(result.Stdout))
	}
	return m.saveLogChunks(ctx, target, checkpoint, chunks...)
}

func (m *AbuseMonitor) saveLogChunks(ctx context.Context, target managedActualizeTarget, checkpoint abuse.Checkpoint, chunks ...string) error {
	complete, partial := splitCompleteLogLines(checkpoint.PartialLine, chunks...)
	if complete == "" {
		checkpoint.PartialLine = partial
		_, err := m.store.SaveEvents(ctx, checkpoint, nil)
		return err
	}
	checkpoint.PartialLine = partial
	events := make([]abuse.Event, 0)
	for _, line := range strings.Split(complete, "\n") {
		event, parseErr := abuse.ParseAccessLine(line, m.config.LogLocation)
		if parseErr != nil {
			continue
		}
		event.PanelID, event.NodeUUID, event.NodeName, event.NodeIP = target.panelID, target.nodeUUID, target.nodeName, target.address
		info := m.lookupIntelligence(ctx, event.ClientIP)
		event.ASN, event.Provider, event.Category = info.ASN, info.Provider, info.Category
		events = append(events, event)
	}
	_, err := m.store.SaveEvents(ctx, checkpoint, events)
	return err
}

func (m *AbuseMonitor) lookupIntelligence(ctx context.Context, ip netip.Addr) abuse.Intelligence {
	now := time.Now().UTC()
	if info, found, err := m.store.LoadIPIntelligence(ctx, ip, now); err == nil && found {
		if m.config.IPInfo == nil || info.ASN != 0 {
			return info
		}
	}
	var info abuse.Intelligence
	if m.config.IPInfo != nil && m.ipInfoLookups.Add(1) <= 20 {
		resolved, err := m.config.IPInfo.Lookup(ctx, ip)
		if err == nil {
			info = resolved
		} else if m.ipInfoErrorLogged.CompareAndSwap(false, true) {
			m.logger.Warn("IPinfo lookup failed; using local fallback", slog.String("ip", ip.String()), slog.Any("error", err))
		}
	}
	if info.ASN == 0 {
		info = m.config.Intelligence.Lookup(ip)
	}
	if override := strings.TrimSpace(m.config.ASNOverrides[info.ASN]); override != "" {
		info.Category = strings.ToUpper(override)
	}
	if err := m.store.SaveIPIntelligence(ctx, ip, info, now.Add(7*24*time.Hour)); err != nil {
		m.logger.Debug("IP intelligence cache write failed", slog.Any("error", err))
	}
	return info
}

func (m *AbuseMonitor) analyze(ctx context.Context) {
	now := time.Now().UTC()
	identities, err := m.store.ListIdentities(ctx, now.Add(-24*time.Hour))
	if err != nil {
		return
	}
	for _, identity := range identities {
		events, loadErr := m.store.LoadEvents(ctx, identity[0], identity[1], now.Add(-24*time.Hour))
		if loadErr != nil {
			continue
		}
		intel := storedEventIntelligence(events)
		observedIPs, fromAPI := m.apiSnapshot(identity[0], identity[1])
		if !fromAPI {
			observedIPs = nil
		}
		assessment := abuse.Assess(identity[0], identity[1], events, intel, observedIPs, now, m.config.Thresholds)
		if assessment.Risk != abuse.RiskLow || assessment.UniqueIPs24H >= m.config.Thresholds.UniqueASNWarn24H {
			m.ipInfoLookups.Store(0)
			intel = enrichUnknownEvents(events, func(ip netip.Addr) abuse.Intelligence {
				return m.lookupIntelligence(ctx, ip)
			})
			if fromAPI {
				for ip := range observedIPs {
					intel[ip] = m.lookupIntelligence(ctx, ip)
				}
			}
			assessment = abuse.Assess(identity[0], identity[1], events, intel, observedIPs, now, m.config.Thresholds)
		}
		if err := m.store.SaveAssessment(ctx, assessment); err != nil {
			m.logger.Warn("abuse assessment persistence failed", slog.String("panel_id", identity[0]), slog.Any("error", err))
			continue
		}
		if assessment.Risk == abuse.RiskLow {
			continue
		}
		incident, notify, openErr := m.store.OpenIncident(ctx, assessment, "mail-and-network-activity", m.config.Cooldown)
		if openErr != nil || !notify {
			continue
		}
		m.notify(ctx, incident, assessment, events)
	}
}

// enrichUnknownEvents refreshes historical UNKNOWN rows in memory before
// scoring and rendering. The persisted access log remains immutable, while a
// single lookup per unique IP is enough to make old incidents useful after
// IPinfo is enabled.
func enrichUnknownEvents(events []abuse.Event, resolve func(netip.Addr) abuse.Intelligence) map[netip.Addr]abuse.Intelligence {
	intel := storedEventIntelligence(events)
	for index := range events {
		ip := events[index].ClientIP
		if !ip.IsValid() {
			continue
		}
		info, found := intel[ip]
		if !found {
			info = resolve(ip)
			intel[ip] = info
		}
		if events[index].ASN == 0 && info.ASN != 0 {
			events[index].ASN, events[index].Provider, events[index].Category = info.ASN, info.Provider, info.Category
		}
	}
	return intel
}

func storedEventIntelligence(events []abuse.Event) map[netip.Addr]abuse.Intelligence {
	intel := make(map[netip.Addr]abuse.Intelligence)
	for _, event := range events {
		if event.ClientIP.IsValid() && event.ASN != 0 {
			intel[event.ClientIP] = abuse.Intelligence{ASN: event.ASN, Provider: event.Provider, Category: event.Category}
		}
	}
	return intel
}

func (m *AbuseMonitor) apiSnapshot(panelID, userID string) (map[netip.Addr]time.Time, bool) {
	m.apiMu.RLock()
	defer m.apiMu.RUnlock()
	if !m.apiPanels[panelID] {
		return nil, false
	}
	result := make(map[netip.Addr]time.Time)
	for ip, seen := range m.apiIPs[[2]string{panelID, userID}] {
		result[ip] = seen
	}
	return result, true
}

func (m *AbuseMonitor) notify(ctx context.Context, incident abuse.Incident, assessment abuse.Assessment, events []abuse.Event) {
	if len(events) == 0 {
		return
	}
	last := events[len(events)-1]
	direct, blocked := 0, 0
	for _, event := range events {
		if event.Blocked {
			blocked++
		} else if event.Outbound == "DIRECT" {
			direct++
		}
	}
	ipSource := "access.log fallback"
	if assessment.IPsFromAPI {
		ipSource = "Remnawave API"
	}
	text := fmt.Sprintf("🚨 ПОДОЗРИТЕЛЬНАЯ АКТИВНОСТЬ\n\nПанель: %s\nНода: %s\nNode IP: %s\n\nUser ID: %s\nSMTP/25 (10m, логи ноды): %d\nSubmission 465/587 (1h, логи ноды): %d\nMail destinations (1h, логи ноды): %d\nBLOCK: %d, DIRECT: %d\nUnique IP (1h/24h, %s): %d/%d\nUnique ASN (24h): %d\n\nTOP client IP:\n%s\n\nRisk: %s (%d)\nПризнаки: %s\n\nЭто оценка косвенных признаков, а не подтверждение отправки спама.", last.PanelID, last.NodeName, last.NodeIP, assessment.UserID, assessment.SMTP25Attempts10M, assessment.SubmissionAttempts1H, assessment.UniqueMailDestinations1H, blocked, direct, ipSource, assessment.UniqueIPs1H, assessment.UniqueIPs24H, assessment.UniqueASNs24H, topIPDetails(events, 5), assessment.Risk, assessment.Score, strings.Join(assessment.Reasons, ", "))
	keyboard := telegram.Keyboard{Inline: [][]telegram.Button{{{Text: "🔎 Подробнее", CallbackData: "abuse:details:" + incident.ID}, {Text: "📊 Статистика", CallbackData: "abuse:stats:" + incident.ID}}, {{Text: "🔕 Игнорировать 24 часа", CallbackData: "abuse:ignore:" + incident.ID}}}}
	for _, ip := range topClientIPs(events, 3) {
		keyboard.Inline = append(keyboard.Inline, []telegram.Button{{Text: "🌐 " + ip, URL: "https://ipinfo.io/" + ip}})
	}
	for _, chatID := range m.allowed {
		if _, err := m.messenger.SendMessage(ctx, chatID, text, keyboard); err != nil {
			m.logger.Warn("abuse alert delivery failed", slog.Int64("chat_id", chatID))
		}
	}
}

func (m *AbuseMonitor) IncidentText(ctx context.Context, id string, stats bool) (string, error) {
	item, err := m.store.GetIncident(ctx, id)
	if err != nil {
		return "", err
	}
	events, err := m.store.LoadEvents(ctx, item.PanelID, item.UserID, time.Now().Add(-24*time.Hour))
	if err != nil {
		return "", err
	}
	if stats {
		ports := map[uint16]int{}
		ips := map[string]int{}
		for _, e := range events {
			ports[e.DestinationPort]++
			ips[e.ClientIP.String()]++
		}
		return fmt.Sprintf("📊 Панель: %s\nUser ID: %s\nСобытий за 24ч: %d\nПорты: %v\nIP: %v", item.PanelID, item.UserID, len(events), ports, topCounts(ips, 5)), nil
	}
	return fmt.Sprintf("🔎 Инцидент %s\nПанель: %s\nUser ID: %s\nRisk: %s (%d)\nПериод: %s — %s UTC\nПричины: %s", item.ID, item.PanelID, item.UserID, item.Risk, item.Score, item.FirstSeen.UTC().Format(time.DateTime), item.LastSeen.UTC().Format(time.DateTime), strings.Join(item.Reasons, ", ")), nil
}
func (m *AbuseMonitor) IgnoreIncident(ctx context.Context, id string) error {
	return m.store.IgnoreIncident(ctx, id, time.Now().Add(24*time.Hour))
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func splitCompleteLogLines(partial string, chunks ...string) (string, string) {
	content := partial + strings.Join(chunks, "")
	lastNewline := strings.LastIndexByte(content, '\n')
	if lastNewline < 0 {
		if len(content) > 64*1024 {
			content = content[len(content)-64*1024:]
		}
		return "", content
	}
	remainder := content[lastNewline+1:]
	if len(remainder) > 64*1024 {
		remainder = ""
	}
	return content[:lastNewline], remainder
}

func topCounts(values map[string]int, limit int) string {
	type pair struct {
		k string
		v int
	}
	items := make([]pair, 0, len(values))
	for k, v := range values {
		items = append(items, pair{k, v})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].v > items[j].v })
	if len(items) > limit {
		items = items[:limit]
	}
	parts := make([]string, len(items))
	for i, item := range items {
		parts[i] = fmt.Sprintf("%s (%d)", item.k, item.v)
	}
	return strings.Join(parts, ", ")
}

func topIPDetails(events []abuse.Event, limit int) string {
	type item struct {
		ip                 string
		count              int
		asn                uint32
		provider, category string
	}
	values := make(map[string]*item)
	for _, event := range events {
		key := event.ClientIP.String()
		current := values[key]
		if current == nil {
			current = &item{ip: key, asn: event.ASN, provider: event.Provider, category: event.Category}
			values[key] = current
		}
		current.count++
	}
	items := make([]*item, 0, len(values))
	for _, current := range values {
		items = append(items, current)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].count > items[j].count })
	if len(items) > limit {
		items = items[:limit]
	}
	lines := make([]string, 0, len(items))
	for _, current := range items {
		asn := "UNKNOWN"
		if current.asn != 0 {
			asn = fmt.Sprintf("AS%d", current.asn)
		}
		lines = append(lines, fmt.Sprintf("• %s — %d, %s, %s, %s", current.ip, current.count, asn, current.provider, current.category))
	}
	return strings.Join(lines, "\n")
}

func topClientIPs(events []abuse.Event, limit int) []string {
	counts := make(map[string]int)
	for _, event := range events {
		if event.ClientIP.IsValid() {
			counts[event.ClientIP.Unmap().String()]++
		}
	}
	type pair struct {
		ip    string
		count int
	}
	items := make([]pair, 0, len(counts))
	for ip, count := range counts {
		items = append(items, pair{ip, count})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].count == items[j].count {
			return items[i].ip < items[j].ip
		}
		return items[i].count > items[j].count
	})
	if len(items) > limit {
		items = items[:limit]
	}
	result := make([]string, len(items))
	for i := range items {
		result[i] = items[i].ip
	}
	return result
}

var _ telegram.AbuseIncidentApplication = (*AbuseMonitor)(nil)
