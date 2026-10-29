CREATE TABLE node_network_inventory (
    panel_id TEXT NOT NULL,
    node_uuid UUID NOT NULL,
    addresses JSONB NOT NULL DEFAULT '[]'::jsonb,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (panel_id, node_uuid),
    CONSTRAINT node_network_inventory_addresses_array CHECK (jsonb_typeof(addresses) = 'array')
);
