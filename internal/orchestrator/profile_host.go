package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"remnanode-setup-bot/internal/certmanager"
	"remnanode-setup-bot/internal/remnawave"
	"remnanode-setup-bot/internal/telegram"
)

var (
	errProfileHostUnavailable = errors.New("profile Host creation is unavailable")
	errProfileHostConflict    = errors.New("a Host with this address already has different settings")
)

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
		return telegram.ProfileHostOptions{}, errors.New("list config profiles failed")
	}
	result := telegram.ProfileHostOptions{}
	for _, profile := range profiles {
		var document profileConfigDocument
		if len(profile.Config) == 0 || json.Unmarshal(profile.Config, &document) != nil {
			continue
		}
		inboundIDs := make(map[string]string, len(profile.Inbounds))
		for _, inbound := range profile.Inbounds {
			inboundIDs[strings.TrimSpace(inbound.Tag)] = inbound.UUID
		}
		for _, inbound := range document.Inbounds {
			inboundID := inboundIDs[strings.TrimSpace(inbound.Tag)]
			port, valid := profileInboundPort(inbound.Port)
			if inboundID == "" || !valid {
				continue
			}
			result.Inbounds = append(result.Inbounds, telegram.ProfileHostInbound{
				ProfileID: profile.UUID, ProfileName: profile.Name,
				InboundID: inboundID, InboundTag: strings.TrimSpace(inbound.Tag), Port: port,
				ServerNames: normalizedServerNames(inbound.StreamSettings.RealitySettings.ServerNames),
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
	if err != nil || len(domain) > 40 {
		return telegram.Host{}, ErrInvalidInput
	}
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
