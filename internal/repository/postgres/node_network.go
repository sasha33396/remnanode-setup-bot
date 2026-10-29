package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sort"
	"strings"

	repositorycontract "remnanode-setup-bot/internal/repository"
)

var _ repositorycontract.NodeNetworkStore = (*Repository)(nil)

func (r *Repository) UpsertNodeNetworkInventory(ctx context.Context, item repositorycontract.NodeNetworkInventory) error {
	item.PanelID = strings.TrimSpace(item.PanelID)
	if !validPanelID(item.PanelID) || !validUUID(item.NodeUUID) {
		return repositorycontract.ErrInvalidArgument
	}
	if len(item.Addresses) > 128 {
		return repositorycontract.ErrInvalidArgument
	}
	unique := make(map[string]struct{}, len(item.Addresses))
	for _, raw := range item.Addresses {
		address, err := netip.ParseAddr(strings.TrimSpace(raw))
		if err != nil {
			return repositorycontract.ErrInvalidArgument
		}
		unique[address.Unmap().String()] = struct{}{}
	}
	item.Addresses = make([]string, 0, len(unique))
	for address := range unique {
		item.Addresses = append(item.Addresses, address)
	}
	sort.Strings(item.Addresses)
	addresses, err := json.Marshal(item.Addresses)
	if err != nil {
		return repositorycontract.ErrInvalidArgument
	}
	_, err = r.pool.Exec(ctx, `
        INSERT INTO node_network_inventory (panel_id, node_uuid, addresses, updated_at)
        VALUES ($1, $2, $3::jsonb, now())
        ON CONFLICT (panel_id, node_uuid) DO UPDATE
        SET addresses = EXCLUDED.addresses, updated_at = now()`, item.PanelID, item.NodeUUID, addresses)
	if err != nil {
		return errors.New("save node network inventory failed")
	}
	return nil
}

func (r *Repository) ListNodeNetworkInventories(ctx context.Context) ([]repositorycontract.NodeNetworkInventory, error) {
	rows, err := r.pool.Query(ctx, `SELECT panel_id, node_uuid::text, addresses, updated_at FROM node_network_inventory`)
	if err != nil {
		return nil, errors.New("list node network inventories failed")
	}
	defer rows.Close()
	var result []repositorycontract.NodeNetworkInventory
	for rows.Next() {
		var item repositorycontract.NodeNetworkInventory
		var addresses []byte
		if err := rows.Scan(&item.PanelID, &item.NodeUUID, &addresses, &item.UpdatedAt); err != nil {
			return nil, errors.New("scan node network inventory failed")
		}
		if err := json.Unmarshal(addresses, &item.Addresses); err != nil {
			return nil, errors.New("decode node network inventory failed")
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("list node network inventories failed")
	}
	return result, nil
}
