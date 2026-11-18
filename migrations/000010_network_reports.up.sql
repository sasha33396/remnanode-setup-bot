CREATE TABLE node_network_reports (
 panel_id text NOT NULL,
 node_uuid uuid NOT NULL,
 report jsonb NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (panel_id, node_uuid)
);
