package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"time"

	"remnanode-setup-bot/internal/certmanager"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/telegram"
)

func (a *TelegramApplication) AuditCertificates(ctx context.Context) (telegram.CertificateAuditResult, error) {
	if a == nil {
		return telegram.CertificateAuditResult{}, errors.New("certificate audit is unavailable")
	}
	a.certificateAuditMu.Lock()
	if a.certificateAuditRunning {
		a.certificateAuditMu.Unlock()
		return telegram.CertificateAuditResult{}, errors.New("certificate audit is already running")
	}
	a.certificateAuditRunning = true
	a.certificateAuditMu.Unlock()
	defer func() {
		a.certificateAuditMu.Lock()
		a.certificateAuditRunning = false
		a.certificateAuditMu.Unlock()
	}()
	result := telegram.CertificateAuditResult{Panels: len(a.order)}
	seen := make(map[string]struct{})
	for _, panelID := range a.order {
		panel := a.panels[panelID]
		hosts, err := panel.Service.remnawave.GetHosts(ctx)
		if err != nil {
			result.Failures = append(result.Failures, telegram.CertificateAuditFailure{PanelName: panel.Name, Reason: "hosts_unavailable"})
			continue
		}
		auditor, ok := panel.Service.certificates.(CertificateAuditor)
		if !ok {
			result.Failures = append(result.Failures, telegram.CertificateAuditFailure{PanelName: panel.Name, Reason: "audit_unavailable"})
			continue
		}
		for _, host := range hosts {
			profile, profileErr := remnawave.DeploymentProfileFromHost(host)
			if profileErr != nil || len(host.Nodes) == 0 {
				continue
			}
			if _, ipErr := netip.ParseAddr(strings.TrimSpace(profile.SNIDomain)); ipErr == nil {
				continue
			}
			domain, normalizeErr := certmanager.NormalizeSNI(profile.SNIDomain)
			if normalizeErr != nil {
				continue
			}
			key := panelID + "\x00" + domain
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result.Hosts++
			audit, auditErr := auditor.Audit(ctx, domain)
			result.Checked += audit.Checked
			result.Healthy += audit.Healthy
			result.Repaired += audit.Repaired
			result.Deferred += audit.Deferred
			if audit.Renewed {
				result.Renewed++
			}
			if auditErr != nil {
				reason := "audit_failed"
				if errors.Is(auditErr, certmanager.ErrDistributionDeferred) {
					reason = "nodes_unreachable"
				} else if message := strings.TrimSpace(certmanager.SafeMessage(auditErr, "")); message != "" {
					reason = message
				}
				result.Failures = append(result.Failures, telegram.CertificateAuditFailure{PanelName: panel.Name, SNI: domain, Reason: reason})
				for _, failure := range audit.Failures {
					slog.Warn("certificate audit target failed", slog.String("panel_id", panelID), slog.String("panel_name", panel.Name), slog.String("sni", domain), slog.String("node_ip", failure.IP.String()), slog.String("failure_kind", string(failure.Kind)), slog.String("reason", failure.Message))
				}
				slog.Warn("certificate audit host failed", slog.String("panel_id", panelID), slog.String("panel_name", panel.Name), slog.String("sni", domain), slog.Int("nodes_checked", audit.Checked), slog.Int("nodes_healthy", audit.Healthy), slog.Int("nodes_repaired", audit.Repaired), slog.Int("nodes_deferred", audit.Deferred), slog.String("reason", reason))
			} else {
				slog.Info("certificate audit host completed", slog.String("panel_id", panelID), slog.String("panel_name", panel.Name), slog.String("sni", domain), slog.Int("nodes_checked", audit.Checked), slog.Int("nodes_healthy", audit.Healthy), slog.Int("nodes_repaired", audit.Repaired), slog.Bool("renewed", audit.Renewed))
			}
		}
	}
	if len(result.Failures) > 0 {
		return result, errors.New("certificate audit completed with failures")
	}
	return result, nil
}

func (a *TelegramApplication) RunCertificateAudits(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("certificate audit interval must be positive")
	}
	run := func() {
		result, err := a.AuditCertificates(ctx)
		attributes := []any{slog.Int("panels", result.Panels), slog.Int("hosts", result.Hosts), slog.Int("nodes_checked", result.Checked), slog.Int("renewed", result.Renewed), slog.Int("repaired", result.Repaired), slog.Int("deferred", result.Deferred), slog.Int("failures", len(result.Failures))}
		if err != nil {
			slog.Warn("planned certificate audit completed with failures", append(attributes, slog.Any("error", err))...)
			return
		}
		slog.Info("planned certificate audit completed", attributes...)
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			run()
		}
	}
}
