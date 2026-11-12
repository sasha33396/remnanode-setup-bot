package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"remnanode-setup-bot/internal/certmanager"
	"remnanode-setup-bot/internal/dnsbalancer"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/telegram"
)

var (
	errProfileHostUnavailable = errors.New("profile Host creation is unavailable")
	errProfileHostConflict    = errors.New("a Host with this address already has different settings")
)

const profileHostDNSTTL = 60

type profileConfigDocument struct {
	Inbounds []profileConfigInbound `json:"inbounds"`
}

type profileConfigInbound struct {
	Tag            string          `json:"tag"`
	Port           json.RawMessage `json:"port"`
	StreamSettings struct {
		RealitySettings struct {
			ServerNames []string `json:"serverNames"`
		} `json:"realitySettings"`
	} `json:"streamSettings"`
}

func (a *TelegramApplication) ListProfileHostOptions(ctx context.Context, panelID string) (telegram.ProfileHostOptions, error) {
	panel, err := a.panel(panelID)
	if err != nil {
		return telegram.ProfileHostOptions{}, err
	}
	api, ok := panel.Service.remnawave.(ProfileHostRemnawaveAPI)
	if !ok {
		return telegram.ProfileHostOptions{}, errProfileHostUnavailable
	}
	profiles, err := api.GetConfigProfiles(ctx)
	if err != nil {
		safeErr, category, status := profileListError(err)
		if panel.Service.logger != nil {
			panel.Service.logger.Warn("profile Host options request failed", "panel_id", panel.ID, "code", safeErr.Code, "category", category, "http_status", status)
		}
		return telegram.ProfileHostOptions{}, safeErr
	}
	result := telegram.ProfileHostOptions{TotalProfiles: len(profiles)}
	for _, profile := range profiles {
		result.TotalInbounds += len(profile.Inbounds)
		var document profileConfigDocument
		_ = unmarshalProfileConfig(profile.Config, &document)
		configByTag := make(map[string]profileConfigInbound, len(document.Inbounds))
		for _, inbound := range document.Inbounds {
			configByTag[strings.TrimSpace(inbound.Tag)] = inbound
		}
		for _, apiInbound := range profile.Inbounds {
			tag := strings.TrimSpace(apiInbound.Tag)
			fallback := configByTag[tag]
			port, valid := apiInboundPort(apiInbound.Port)
			if !valid {
				port, valid = rawInboundPort(apiInbound.RawInbound)
			}
			if !valid {
				port, valid = profileInboundPort(fallback.Port)
			}
			if apiInbound.UUID == "" || !valid {
				if !valid {
					result.SkippedWithoutPort++
				}
				continue
			}
			serverNames := rawInboundServerNames(apiInbound.RawInbound)
			if len(serverNames) == 0 {
				serverNames = normalizedServerNames(fallback.StreamSettings.RealitySettings.ServerNames)
			}
			result.Inbounds = append(result.Inbounds, telegram.ProfileHostInbound{
				ProfileID: profile.UUID, ProfileName: profile.Name,
				InboundID: apiInbound.UUID, InboundTag: tag, Port: port, ServerNames: serverNames,
			})
		}
	}
	sort.Slice(result.Inbounds, func(i, j int) bool {
		left, right := result.Inbounds[i], result.Inbounds[j]
		if left.ProfileName != right.ProfileName {
			return left.ProfileName < right.ProfileName
		}
		return left.InboundTag < right.InboundTag
	})
	return result, nil
}

func profileListError(err error) (*SafeError, string, int) {
	message, code, category, status := "Не удалось загрузить Config Profiles из Remnawave.", "PROFILE_LIST_FAILED", "transport", 0
	if errors.Is(err, remnawave.ErrUnauthorized) {
		message, code, category = "Remnawave API запретил чтение профилей. Проверьте scope config-profiles:list у токена панели.", "PROFILE_LIST_FORBIDDEN", "authorization"
	} else if errors.Is(err, remnawave.ErrInvalidResponse) {
		message, code, category = "Remnawave вернул несовместимый ответ списка Config Profiles.", "PROFILE_LIST_INVALID_RESPONSE", "invalid_response"
	} else {
		var httpErr *remnawave.HTTPError
		if errors.As(err, &httpErr) {
			status, category = httpErr.StatusCode, "http"
			message, code = fmt.Sprintf("Remnawave API вернул HTTP %d при чтении Config Profiles.", status), "PROFILE_LIST_HTTP_ERROR"
		} else if errors.Is(err, context.DeadlineExceeded) {
			message, code, category = "Remnawave API не успел вернуть Config Profiles до таймаута.", "PROFILE_LIST_TIMEOUT", "timeout"
		} else if errors.Is(err, context.Canceled) {
			message, code, category = "Запрос Config Profiles был отменён.", "PROFILE_LIST_CANCELLED", "cancelled"
		}
	}
	return &SafeError{Code: code, SafeMessage: message, Kind: err}, category, status
}

func unmarshalProfileConfig(raw json.RawMessage, target *profileConfigDocument) bool {
	if len(raw) == 0 || target == nil {
		return false
	}
	if json.Unmarshal(raw, target) == nil {
		return true
	}
	var encoded string
	return json.Unmarshal(raw, &encoded) == nil && json.Unmarshal([]byte(encoded), target) == nil
}

func apiInboundPort(port *int) (int, bool) {
	if port != nil && *port >= 1 && *port <= 65535 {
		return *port, true
	}
	return 0, false
}

func rawInboundPort(raw json.RawMessage) (int, bool) {
	var inbound profileConfigInbound
	if !unmarshalRawInbound(raw, &inbound) {
		return 0, false
	}
	return profileInboundPort(inbound.Port)
}

func rawInboundServerNames(raw json.RawMessage) []string {
	var inbound profileConfigInbound
	if !unmarshalRawInbound(raw, &inbound) {
		return nil
	}
	return normalizedServerNames(inbound.StreamSettings.RealitySettings.ServerNames)
}

func unmarshalRawInbound(raw json.RawMessage, target *profileConfigInbound) bool {
	if len(raw) == 0 || string(raw) == "null" || target == nil {
		return false
	}
	if json.Unmarshal(raw, target) == nil {
		return true
	}
	var encoded string
	return json.Unmarshal(raw, &encoded) == nil && json.Unmarshal([]byte(encoded), target) == nil
}

func (a *TelegramApplication) EnsureProfileHost(ctx context.Context, input telegram.ProfileHostCreateInput) (telegram.Host, error) {
	panel, err := a.panel(input.PanelID)
	if err != nil {
		return telegram.Host{}, err
	}
	api, ok := panel.Service.remnawave.(ProfileHostRemnawaveAPI)
	if !ok {
		return telegram.Host{}, errProfileHostUnavailable
	}
	domain, err := certmanager.NormalizeSNI(input.Domain)
	if err != nil || len(domain) > 40 || !input.VPSIP.IsValid() {
		return telegram.Host{}, ErrInvalidInput
	}
	input.VPSIP = input.VPSIP.Unmap()
	options, err := a.ListProfileHostOptions(ctx, input.PanelID)
	if err != nil {
		return telegram.Host{}, err
	}
	var selected *telegram.ProfileHostInbound
	for index := range options.Inbounds {
		option := &options.Inbounds[index]
		if option.ProfileID == strings.TrimSpace(input.ProfileID) && option.InboundID == strings.TrimSpace(input.InboundID) {
			selected = option
			break
		}
	}
	if selected == nil {
		return telegram.Host{}, ErrHostUnavailable
	}
	hosts, err := panel.Service.remnawave.GetHosts(ctx)
	if err != nil {
		return telegram.Host{}, errors.New("list Hosts failed")
	}
	var reusable *remnawave.Host
	conflict := false
	for index := range hosts {
		host := &hosts[index]
		hostDomain, normalizeErr := certmanager.NormalizeSNI(host.Address)
		if normalizeErr != nil || hostDomain != domain {
			continue
		}
		matches := !host.IsDisabled && !host.IsHidden && remnawave.IsProfileHost(*host) && host.Inbound.ConfigProfileUUID != nil && host.Inbound.ConfigProfileInboundUUID != nil && *host.Inbound.ConfigProfileUUID == selected.ProfileID && *host.Inbound.ConfigProfileInboundUUID == selected.InboundID
		if matches && reusable == nil {
			reusable = host
			continue
		}
		conflict = true
	}
	if conflict {
		return telegram.Host{}, errProfileHostConflict
	}
	if !panel.DNSEnabled {
		return telegram.Host{}, safeError("PROFILE_HOST_DNS_DISABLED", "Для этой панели DNS-балансировщик отключён; создать поддомен автоматически нельзя.", ErrDNSUpdateFailed)
	}
	if _, err := panel.Service.dns.EnsureZone(ctx, domain, input.VPSIP, profileHostDNSTTL); err != nil {
		if panel.Service.logger != nil {
			panel.Service.logger.Warn("profile Host DNS zone ensure failed", "panel_id", panel.ID, "fqdn", domain, "category", profileHostDNSErrorCategory(err), "http_status", dnsHTTPStatus(err))
		}
		return telegram.Host{}, profileHostDNSError(err)
	}
	if reusable != nil {
		return profileHostProjection(*reusable), nil
	}
	created, err := api.CreateVisibleHost(ctx, remnawave.CreateVisibleHostInput{
		Remark: domain, Address: domain, Port: selected.Port,
		ConfigProfileUUID: selected.ProfileID, InboundUUID: selected.InboundID,
	})
	if err != nil {
		return telegram.Host{}, errors.New("create Host failed")
	}
	return profileHostProjection(created), nil
}

func profileHostDNSError(err error) error {
	message, code := "Не удалось создать DNS-поддомен через DNS-балансировщик.", "PROFILE_HOST_DNS_FAILED"
	switch {
	case errors.Is(err, dnsbalancer.ErrUnauthorized):
		message, code = "DNS-балансировщик отклонил API-ключ. Проверьте DNS API token панели.", "PROFILE_HOST_DNS_FORBIDDEN"
	case errors.Is(err, dnsbalancer.ErrNotFound):
		message, code = "Родительский домен не настроен в remnawave-cloudflare-nodes. Сначала добавьте его в domains.", "PROFILE_HOST_DNS_PARENT_NOT_FOUND"
	case errors.Is(err, dnsbalancer.ErrUnprocessable):
		message, code = "DNS-балансировщик отклонил параметры нового поддомена.", "PROFILE_HOST_DNS_INVALID"
	case errors.Is(err, dnsbalancer.ErrInvalidResponse):
		message, code = "DNS-балансировщик вернул некорректный ответ при создании поддомена.", "PROFILE_HOST_DNS_INVALID_RESPONSE"
	default:
		if status := dnsHTTPStatus(err); status != 0 {
			message, code = fmt.Sprintf("DNS-балансировщик вернул HTTP %d при создании поддомена.", status), "PROFILE_HOST_DNS_HTTP_ERROR"
		}
	}
	return safeError(code, message, ErrDNSUpdateFailed)
}

func profileHostDNSErrorCategory(err error) string {
	switch {
	case errors.Is(err, dnsbalancer.ErrUnauthorized):
		return "authorization"
	case errors.Is(err, dnsbalancer.ErrNotFound):
		return "parent_not_found"
	case errors.Is(err, dnsbalancer.ErrUnprocessable):
		return "validation"
	case errors.Is(err, dnsbalancer.ErrInvalidResponse):
		return "invalid_response"
	default:
		if dnsHTTPStatus(err) != 0 {
			return "http"
		}
		return "transport"
	}
}

func dnsHTTPStatus(err error) int {
	var httpErr *dnsbalancer.HTTPError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode
	}
	return 0
}

func profileInboundPort(raw json.RawMessage) (int, bool) {
	var number int
	if json.Unmarshal(raw, &number) == nil && number >= 1 && number <= 65535 {
		return number, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		parsed, err := strconv.Atoi(strings.TrimSpace(text))
		if err == nil && parsed >= 1 && parsed <= 65535 {
			return parsed, true
		}
	}
	return 0, false
}

func normalizedServerNames(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		domain, err := certmanager.NormalizeSNI(value)
		if err != nil || len(domain) > 40 {
			continue
		}
		if _, exists := seen[domain]; exists {
			continue
		}
		seen[domain] = struct{}{}
		result = append(result, domain)
	}
	sort.Strings(result)
	return result
}

func profileHostProjection(host remnawave.Host) telegram.Host {
	return telegram.Host{ID: host.UUID, Remark: host.Remark, Address: host.Address, ConfigProfileReadiness: telegram.ReadinessReady}
}
