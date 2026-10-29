package repository

import (
	"context"
	"time"
)

// NodeNetworkInventory stores only non-secret addresses discovered in netplan.
type NodeNetworkInventory struct {
	PanelID   string
	NodeUUID  string
	Addresses []string
	UpdatedAt time.Time
}

type NodeNetworkStore interface {
	UpsertNodeNetworkInventory(context.Context, NodeNetworkInventory) error
	ListNodeNetworkInventories(context.Context) ([]NodeNetworkInventory, error)
}
