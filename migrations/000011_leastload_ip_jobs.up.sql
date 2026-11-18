CREATE TABLE leastload_ip_jobs (
 id uuid PRIMARY KEY,
 job jsonb NOT NULL,
 updated_at timestamptz NOT NULL DEFAULT now()
);
