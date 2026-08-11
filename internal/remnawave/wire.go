package remnawave

import "time"

type hostsEnvelope struct {
	Response *[]hostWire `json:"response"`
}

type hostWire struct {
	UUID       *string          `json:"uuid"`
	Remark     *string          `json:"remark"`
	Address    *string          `json:"address"`
	SNI        *string          `json:"sni"`
	IsDisabled *bool            `json:"isDisabled"`
	Inbound    *hostInboundWire `json:"inbound"`
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
		UUID:       *h.UUID,
		Remark:     *h.Remark,
		Address:    *h.Address,
		SNI:        h.SNI,
		IsDisabled: *h.IsDisabled,
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

type updateNodeAddressRequest struct {
	UUID    string `json:"uuid"`
	Address string `json:"address"`
}

type deleteNodeEnvelope struct {
	Response *struct {
		IsDeleted *bool `json:"isDeleted"`
	} `json:"response"`
}
