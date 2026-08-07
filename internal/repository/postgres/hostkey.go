package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	repositorycontract "remnanode-setup-bot/internal/repository"
	sshclient "remnanode-setup-bot/internal/ssh"
)

var _ sshclient.HostKeyStore = (*Repository)(nil)

// Get loads the fingerprint associated with a deployment for SSH TOFU.
func (r *Repository) Get(ctx context.Context, deploymentID string) (string, bool, error) {
	if !validUUID(deploymentID) {
		return "", false, invalid("deployment ID")
	}
	var fingerprint *string
	err := r.pool.QueryRow(ctx, `
        SELECT ssh_host_key_fingerprint
        FROM deployments
        WHERE id = $1`, deploymentID).Scan(&fingerprint)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("get SSH host key: %w", repositorycontract.ErrNotFound)
	}
	if err != nil {
		return "", false, fmt.Errorf("get SSH host key: %w", err)
	}
	if fingerprint == nil {
		return "", false, nil
	}
	return *fingerprint, true, nil
}

// StoreIfAbsent atomically pins the first fingerprint and returns the already
// trusted value on subsequent calls.
func (r *Repository) StoreIfAbsent(ctx context.Context, deploymentID, fingerprint string) (string, bool, error) {
	if !validUUID(deploymentID) {
		return "", false, invalid("deployment ID")
	}
	if !strings.HasPrefix(fingerprint, "SHA256:") || len(fingerprint) <= len("SHA256:") {
		return "", false, invalid("SSH host key fingerprint")
	}
	var trusted string
	var stored bool
	err := r.pool.QueryRow(ctx, `
        WITH current AS MATERIALIZED (
            SELECT ssh_host_key_fingerprint
            FROM deployments
            WHERE id = $1
            FOR UPDATE
        ), updated AS (
            UPDATE deployments AS deployment
            SET ssh_host_key_fingerprint = COALESCE(current.ssh_host_key_fingerprint, $2),
                updated_at = now()
            FROM current
            WHERE deployment.id = $1
            RETURNING deployment.ssh_host_key_fingerprint,
                      current.ssh_host_key_fingerprint IS NULL AS stored
        )
        SELECT ssh_host_key_fingerprint, stored FROM updated`,
		deploymentID, fingerprint).Scan(&trusted, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("store SSH host key: %w", repositorycontract.ErrNotFound)
	}
	if err != nil {
		return "", false, fmt.Errorf("store SSH host key: %w", err)
	}
	return trusted, stored, nil
}
