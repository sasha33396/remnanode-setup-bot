package remnawave

import (
	"encoding/json"
	"time"
)

type hostsEnvelope struct {
	Response *[]hostWire `json:"response"`
}

type hostWire struct {
	UUID                 *string          `json:"uuid"`
	Remark               *string          `json:"remark"`
	Address              *string          `json:"address"`
	SNI                  *string          `json:"sni"`
	IsDisabled           *bool            `json:"isDisabled"`
	IsHidden             *bool            `json:"isHidden"`
	Tags                 []string         `json:"tags"`
	Nodes                []string         `json:"nodes"`
	XrayJSONTemplateUUID *string          `json:"xrayJsonTemplateUuid"`
	Inbound              *hostInboundWire `json:"inbound"`
}

type hostInboundWire struct {
	ConfigProfileUUID        *string `json:"configProfileUuid"`
	ConfigProfileInboundUUID *string `json:"configProfileInboundUuid"`
}

func (h hostWire) model() (Host, error) {
	if h.UUID == nil || !validUUID(*h.UUID) || h.Remark == nil || h.Address == nil || *h.Address == "" || h.IsDisabled == nil || h.Inbound == nil {
		return Host{}, ErrInvalidResponse
	}
	return Host{
		UUID:                 *h.UUID,
		Remark:               *h.Remark,
		Address:              *h.Address,
		SNI:                  h.SNI,
		IsDisabled:           *h.IsDisabled,
		IsHidden:             h.IsHidden != nil && *h.IsHidden,
		Tags:                 append([]string(nil), h.Tags...),
		Nodes:                append([]string(nil), h.Nodes...),
		XrayJSONTemplateUUID: h.XrayJSONTemplateUUID,
		Inbound: HostInbound{
			ConfigProfileUUID:        h.Inbound.ConfigProfileUUID,
			ConfigProfileInboundUUID: h.Inbound.ConfigProfileInboundUUID,
		},
	}, nil
}

type keygenEnvelope struct {
	Response *struct {
		PubKey *string `json:"pubKey"`
	} `json:"response"`
}

type nodesEnvelope struct {
	Response *[]nodeWire `json:"response"`
}

type nodeEnvelope struct {
	Response *nodeWire `json:"response"`
}

type nodeMetricsEnvelope struct {
	Response *struct {
		Nodes *[]nodeMetricWire `json:"nodes"`
	} `json:"response"`
}

type nodeMetricWire struct {
	NodeUUID    *string `json:"nodeUuid"`
	NodeName    *string `json:"nodeName"`
	UsersOnline *int    `json:"usersOnline"`
}

func (m nodeMetricWire) model() (NodeMetric, error) {
	if m.NodeUUID == nil || !validUUID(*m.NodeUUID) || m.NodeName == nil || *m.NodeName == "" || m.UsersOnline == nil || *m.UsersOnline < 0 {
		return NodeMetric{}, ErrInvalidResponse
	}
	return NodeMetric{NodeUUID: *m.NodeUUID, NodeName: *m.NodeName, UsersOnline: *m.UsersOnline}, nil
}

type nodeWire struct {
	UUID              *string               `json:"uuid"`
	Name              *string               `json:"name"`
	Address           *string               `json:"address"`
	Port              *int                  `json:"port"`
	IsConnected       *bool                 `json:"isConnected"`
	IsDisabled        *bool                 `json:"isDisabled"`
	IsConnecting      *bool                 `json:"isConnecting"`
	LastStatusChange  *time.Time            `json:"lastStatusChange"`
	LastStatusMessage *string               `json:"lastStatusMessage"`
	ConfigProfile     nodeConfigProfileWire `json:"configProfile"`
}

type nodeConfigProfileWire struct {
	ActiveConfigProfileUUID *string             `json:"activeConfigProfileUuid"`
	ActiveInbounds          []activeInboundWire `json:"activeInbounds"`
}

type activeInboundWire struct {
	UUID string `json:"uuid"`
}

func (n nodeWire) model() (Node, error) {
	if n.UUID == nil || !validUUID(*n.UUID) || n.Name == nil || *n.Name == "" || n.Address == nil || *n.Address == "" || n.IsConnected == nil || n.IsDisabled == nil || n.IsConnecting == nil {
		return Node{}, ErrInvalidResponse
	}
	inbounds := make([]string, 0, len(n.ConfigProfile.ActiveInbounds))
	for _, inbound := range n.ConfigProfile.ActiveInbounds {
		inbounds = append(inbounds, inbound.UUID)
	}
	return Node{
		UUID:                    *n.UUID,
		Name:                    *n.Name,
		Address:                 *n.Address,
		Port:                    n.Port,
		IsConnected:             *n.IsConnected,
		IsConnecting:            *n.IsConnecting,
		IsDisabled:              *n.IsDisabled,
		LastStatusMessage:       n.LastStatusMessage,
		LastStatusChange:        n.LastStatusChange,
		ActiveConfigProfileUUID: n.ConfigProfile.ActiveConfigProfileUUID,
		ActiveInboundUUIDs:      inbounds,
	}, nil
}

type createNodeRequest struct {
	Name          string            `json:"name"`
	Address       string            `json:"address"`
	Port          int               `json:"port"`
	ConfigProfile NodeConfigProfile `json:"configProfile"`
}

type configProfilesEnvelope struct {
	Response *struct {
		ConfigProfiles []configProfileWire `json:"configProfiles"`
	} `json:"response"`
}

type configProfileEnvelope struct {
	Response *configProfileWire `json:"response"`
}

type configProfileWire struct {
	UUID     *string                    `json:"uuid"`
	Name     *string                    `json:"name"`
	Config   json.RawMessage            `json:"config"`
	Inbounds []configProfileInboundWire `json:"inbounds"`
}

type configProfileInboundWire struct {
	UUID *string `json:"uuid"`
	Tag  *string `json:"tag"`
}

func (p configProfileWire) model() (ConfigProfile, error) {
	if p.UUID == nil || !validUUID(*p.UUID) || p.Name == nil || *p.Name == "" || len(p.Config) == 0 {
		return ConfigProfile{}, ErrInvalidResponse
	}
	result := ConfigProfile{UUID: *p.UUID, Name: *p.Name, Config: append(json.RawMessage(nil), p.Config...)}
	for _, inbound := range p.Inbounds {
		if inbound.UUID == nil || !validUUID(*inbound.UUID) || inbound.Tag == nil || *inbound.Tag == "" {
			return ConfigProfile{}, ErrInvalidResponse
		}
		result.Inbounds = append(result.Inbounds, ConfigProfileInbound{UUID: *inbound.UUID, Tag: *inbound.Tag})
	}
	return result, nil
}

type internalSquadsEnvelope struct {
	Response *struct {
		InternalSquads []internalSquadWire `json:"internalSquads"`
	} `json:"response"`
}

type internalSquadEnvelope struct {
	Response *internalSquadWire `json:"response"`
}

type internalSquadWire struct {
	UUID     *string                    `json:"uuid"`
	Name     *string                    `json:"name"`
	Inbounds []configProfileInboundWire `json:"inbounds"`
}

func (s internalSquadWire) model() (InternalSquad, error) {
	if s.UUID == nil || !validUUID(*s.UUID) || s.Name == nil || *s.Name == "" {
		return InternalSquad{}, ErrInvalidResponse
	}
	result := InternalSquad{UUID: *s.UUID, Name: *s.Name}
	for _, inbound := range s.Inbounds {
		if inbound.UUID == nil || !validUUID(*inbound.UUID) {
			return InternalSquad{}, ErrInvalidResponse
		}
		result.Inbounds = append(result.Inbounds, *inbound.UUID)
	}
	return result, nil
}

type x25519Envelope struct {
	Response *struct {
		Keypairs []X25519Keypair `json:"keypairs"`
	} `json:"response"`
}

type createConfigProfileRequest struct {
	Name   string `json:"name"`
	Config any    `json:"config"`
}

type createHostRequest struct {
	Inbound struct {
		ConfigProfileUUID string `json:"configProfileUuid"`
		InboundUUID       string `json:"configProfileInboundUuid"`
	} `json:"inbound"`
	Remark     string   `json:"remark"`
	Address    string   `json:"address"`
	Port       int      `json:"port"`
	Tags       []string `json:"tags"`
	IsHidden   bool     `json:"isHidden"`
	IsDisabled bool     `json:"isDisabled"`
	Nodes      []string `json:"nodes"`
}

type updateInternalSquadRequest struct {
	UUID     string   `json:"uuid"`
	Inbounds []string `json:"inbounds"`
}

type updateNodeAddressRequest struct {
	UUID    string `json:"uuid"`
	Address string `json:"address"`
}

type setNodeDisabledRequest struct {
	UUID       string `json:"uuid"`
	IsDisabled bool   `json:"isDisabled"`
}

type updateNodeProfileRequest struct {
	UUID          string            `json:"uuid"`
	ConfigProfile NodeConfigProfile `json:"configProfile"`
}

type deleteNodeEnvelope struct {
	Response *struct {
		IsDeleted *bool `json:"isDeleted"`
	} `json:"response"`
}
