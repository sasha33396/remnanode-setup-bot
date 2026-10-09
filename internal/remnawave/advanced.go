package remnawave

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
)

var (
	hostTagPattern           = regexp.MustCompile(`^[A-Z0-9_:]+$`)
	configProfileNamePattern = regexp.MustCompile(`^[A-Za-z0-9_\s-]+$`)
)

func (c *Client) GetConfigProfiles(ctx context.Context) ([]ConfigProfile, error) {
	var envelope configProfilesEnvelope
	if err := c.doJSON(ctx, http.MethodGet, "/api/config-profiles", nil, http.StatusOK, &envelope); err != nil {
		return nil, err
	}
	if envelope.Response == nil {
		return nil, invalidResponse("config profiles response is missing")
	}
	result := make([]ConfigProfile, 0, len(envelope.Response.ConfigProfiles))
	for index, item := range envelope.Response.ConfigProfiles {
		profile, err := item.model()
		if err != nil {
			return nil, invalidResponse(fmt.Sprintf("config profile %d is incomplete", index))
		}
		result = append(result, profile)
	}
	return result, nil
}

func (c *Client) CreateConfigProfile(ctx context.Context, input CreateConfigProfileInput) (ConfigProfile, error) {
	name := strings.TrimSpace(input.Name)
	if len(name) < 2 || len(name) > 30 || !configProfileNamePattern.MatchString(name) || input.Config == nil {
		return ConfigProfile{}, fmt.Errorf("config profile: %w", ErrInvalidInput)
	}
	var envelope configProfileEnvelope
	request := createConfigProfileRequest{Name: name, Config: input.Config}
	if err := c.doJSON(ctx, http.MethodPost, "/api/config-profiles", request, http.StatusCreated, &envelope); err != nil {
		return ConfigProfile{}, err
	}
	if envelope.Response == nil {
		return ConfigProfile{}, invalidResponse("created config profile response is missing")
	}
	profile, err := envelope.Response.model()
	if err != nil || len(profile.Inbounds) != 1 {
		return ConfigProfile{}, invalidResponse("created config profile must contain one inbound")
	}
	return profile, nil
}

func (c *Client) GenerateX25519(ctx context.Context) (X25519Keypair, error) {
	var envelope x25519Envelope
	if err := c.doJSON(ctx, http.MethodGet, "/api/system/tools/x25519/generate", nil, http.StatusOK, &envelope); err != nil {
		return X25519Keypair{}, err
	}
	if envelope.Response == nil || len(envelope.Response.Keypairs) == 0 {
		return X25519Keypair{}, invalidResponse("X25519 keypairs response is missing")
	}
	pair := envelope.Response.Keypairs[0]
	if strings.TrimSpace(pair.PrivateKey) == "" || strings.TrimSpace(pair.PublicKey) == "" {
		return X25519Keypair{}, invalidResponse("X25519 keypair is incomplete")
	}
	return pair, nil
}

func (c *Client) GetInternalSquads(ctx context.Context) ([]InternalSquad, error) {
	var envelope internalSquadsEnvelope
	if err := c.doJSON(ctx, http.MethodGet, "/api/internal-squads", nil, http.StatusOK, &envelope); err != nil {
		return nil, err
	}
	if envelope.Response == nil {
		return nil, invalidResponse("internal squads response is missing")
	}
	result := make([]InternalSquad, 0, len(envelope.Response.InternalSquads))
	for index, item := range envelope.Response.InternalSquads {
		squad, err := item.model()
		if err != nil {
			return nil, invalidResponse(fmt.Sprintf("internal squad %d is incomplete", index))
		}
		result = append(result, squad)
	}
	return result, nil
}

func (c *Client) AddInboundToInternalSquad(ctx context.Context, squadUUID, inboundUUID string) (InternalSquad, error) {
	if !validUUID(squadUUID) || !validUUID(inboundUUID) {
		return InternalSquad{}, fmt.Errorf("internal squad or inbound UUID: %w", ErrInvalidInput)
	}
	c.squadMu.Lock()
	defer c.squadMu.Unlock()
	squads, err := c.GetInternalSquads(ctx)
	if err != nil {
		return InternalSquad{}, err
	}
	for _, squad := range squads {
		if squad.UUID != squadUUID {
			continue
		}
		for _, existing := range squad.Inbounds {
			if existing == inboundUUID {
				return squad, nil
			}
		}
		request := updateInternalSquadRequest{UUID: squadUUID, Inbounds: append(append([]string(nil), squad.Inbounds...), inboundUUID)}
		var envelope internalSquadEnvelope
		if err := c.doJSON(ctx, http.MethodPatch, "/api/internal-squads", request, http.StatusOK, &envelope); err != nil {
			return InternalSquad{}, err
		}
		if envelope.Response == nil {
			return InternalSquad{}, invalidResponse("updated internal squad response is missing")
		}
		return envelope.Response.model()
	}
	return InternalSquad{}, fmt.Errorf("internal squad: %w", ErrInvalidInput)
}

func (c *Client) CreateHost(ctx context.Context, input CreateHostInput) (Host, error) {
	input.Remark, input.Address, input.Tag = strings.TrimSpace(input.Remark), strings.TrimSpace(input.Address), strings.TrimSpace(input.Tag)
	if input.Remark == "" || len(input.Remark) > 40 || input.Address == "" || input.Port < 1 || input.Port > 65535 || !hostTagPattern.MatchString(input.Tag) || len(input.Tag) > 36 || !validUUID(input.ConfigProfileUUID) || !validUUID(input.InboundUUID) || !validUUID(input.NodeUUID) {
		return Host{}, fmt.Errorf("Host: %w", ErrInvalidInput)
	}
	request := createHostRequest{Remark: input.Remark, Address: input.Address, Port: input.Port, Tags: []string{input.Tag}, IsHidden: true, Nodes: []string{input.NodeUUID}}
	request.Inbound.ConfigProfileUUID = input.ConfigProfileUUID
	request.Inbound.InboundUUID = input.InboundUUID
	var envelope struct {
		Response *hostWire `json:"response"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/hosts", request, http.StatusCreated, &envelope); err != nil {
		return Host{}, err
	}
	if envelope.Response == nil {
		return Host{}, invalidResponse("created Host response is missing")
	}
	return envelope.Response.model()
}

// AdvancedBalancerConfig builds the one-inbound server profile used by each
// injected Host. sendThrough is intentionally omitted in single-IP mode.
func AdvancedBalancerConfig(inboundTag, fqdn, privateKey string, egressIP *netip.Addr) map[string]any {
	direct := map[string]any{"tag": "DIRECT", "protocol": "freedom"}
	if egressIP != nil && egressIP.IsValid() {
		direct["sendThrough"] = egressIP.Unmap().String()
	}
	return map[string]any{
		"log": map[string]any{"error": "/var/log/remnanode/error.log", "access": "/var/log/remnanode/access.log", "loglevel": "warning"},
		"inbounds": []any{map[string]any{
			"tag": inboundTag, "port": 443, "listen": "0.0.0.0", "protocol": "vless",
			"settings": map[string]any{"flow": "xtls-rprx-vision", "clients": []any{}, "decryption": "none"},
			"sniffing": map[string]any{"enabled": true, "destOverride": []string{"http", "tls", "quic"}},
			"streamSettings": map[string]any{"network": "raw", "security": "reality", "realitySettings": map[string]any{
				"show": false, "xver": 0, "target": "127.0.0.1:9443", "shortIds": []string{""}, "privateKey": privateKey, "fingerprint": "qq", "serverNames": []string{fqdn}, "minClientVer": "0.0.0",
			}},
		}},
		"outbounds": []any{direct, map[string]any{"tag": "BLOCK", "protocol": "blackhole"}},
		"routing": map[string]any{"rules": []any{
			map[string]any{"ip": []string{"geoip:private"}, "type": "field", "outboundTag": "BLOCK"},
			map[string]any{"domain": []string{"geosite:private"}, "type": "field", "outboundTag": "BLOCK"},
			map[string]any{"protocol": []string{"bittorrent"}, "type": "field", "outboundTag": "BLOCK"},
			map[string]any{"port": "25", "type": "field", "network": "tcp", "outboundTag": "BLOCK"},
		}},
	}
}
