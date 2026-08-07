package postgres

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"remnanode-setup-bot/internal/certmanager"
	"remnanode-setup-bot/internal/deployment"
	repositorycontract "remnanode-setup-bot/internal/repository"
	"remnanode-setup-bot/migrations"
)

func TestRepositoryIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	adminPool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("create admin pool: %v", err)
	}
	t.Cleanup(adminPool.Close)

	randomID, err := deployment.NewID()
	if err != nil {
		t.Fatalf("create schema ID: %v", err)
	}
	schema := "repository_test_" + strings.ReplaceAll(randomID, "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := adminPool.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if _, err := adminPool.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse pool config: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("create repository pool: %v", err)
	}
	defer pool.Close()

	for _, migration := range []string{"000001_deployments.up.sql", "000002_deployment_ssh_host_key.up.sql", "000003_certificate_manager.up.sql", "000004_production_recovery.up.sql"} {
		upSQL, err := migrations.Files.ReadFile(migration)
		if err != nil {
			t.Fatalf("read migration %s: %v", migration, err)
		}
		if _, err := pool.Exec(ctx, string(upSQL)); err != nil {
			t.Fatalf("apply migration %s: %v", migration, err)
		}
	}

	assertNoRootPasswordColumn(t, ctx, pool)
	repo := New(pool)

	first := createTestDeployment(t, ctx, repo, "node-1", "192.0.2.10")
	fingerprint := "SHA256:deployment-host-key"
	trusted, stored, err := repo.StoreIfAbsent(ctx, first.ID, fingerprint)
	if err != nil || !stored || trusted != fingerprint {
		t.Fatalf("StoreIfAbsent() = %q, %t, %v", trusted, stored, err)
	}
	trusted, found, err := repo.Get(ctx, first.ID)
	if err != nil || !found || trusted != fingerprint {
		t.Fatalf("Get(host key) = %q, %t, %v", trusted, found, err)
	}
	otherFingerprint := "SHA256:different-host-key"
	trusted, stored, err = repo.StoreIfAbsent(ctx, first.ID, otherFingerprint)
	if err != nil || stored || trusted != fingerprint {
		t.Fatalf("StoreIfAbsent(existing) = %q, %t, %v", trusted, stored, err)
	}
	loaded, err := repo.GetDeployment(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetDeployment() error = %v", err)
	}
	if loaded.NodeName != "node-1" || loaded.Status != deployment.StatusCreated {
		t.Fatalf("GetDeployment() = %#v", loaded)
	}
	if loaded.SSHHostKeyFingerprint == nil || *loaded.SSHHostKeyFingerprint != fingerprint {
		t.Fatalf("persisted SSH fingerprint = %v", loaded.SSHHostKeyFingerprint)
	}

	running, err := repo.UpdateDeploymentState(ctx, first.ID, repositorycontract.UpdateDeploymentStateParams{
		Status:      deployment.StatusProvisioning,
		CurrentStep: "docker",
	})
	if err != nil {
		t.Fatalf("UpdateDeploymentState() error = %v", err)
	}
	if running.StartedAt == nil || running.CompletedAt != nil {
		t.Fatalf("running lifecycle timestamps = started %v, completed %v", running.StartedAt, running.CompletedAt)
	}

	nodeUUID := "c55c6980-ac8e-4c6a-8579-dd3df8a29891"
	withNode, err := repo.SetRemnawaveNodeUUID(ctx, first.ID, nodeUUID)
	if err != nil {
		t.Fatalf("SetRemnawaveNodeUUID() error = %v", err)
	}
	if withNode.RemnawaveNodeUUID == nil || *withNode.RemnawaveNodeUUID != nodeUUID {
		t.Fatalf("Remnawave node UUID = %v, want %s", withNode.RemnawaveNodeUUID, nodeUUID)
	}

	step, err := repo.RecordDeploymentStep(ctx, repositorycontract.RecordStepParams{
		DeploymentID: first.ID,
		Name:         "docker",
		Status:       deployment.StepStatusRunning,
	})
	if err != nil {
		t.Fatalf("RecordDeploymentStep(RUNNING) error = %v", err)
	}
	if step.StartedAt == nil || step.CompletedAt != nil {
		t.Fatalf("running step timestamps = started %v, completed %v", step.StartedAt, step.CompletedAt)
	}

	summary := "Docker is installed"
	step, err = repo.RecordDeploymentStep(ctx, repositorycontract.RecordStepParams{
		DeploymentID: first.ID,
		Name:         "docker",
		Status:       deployment.StepStatusCompleted,
		SafeSummary:  &summary,
	})
	if err != nil {
		t.Fatalf("RecordDeploymentStep(COMPLETED) error = %v", err)
	}
	if step.CompletedAt == nil || step.SafeSummary == nil || *step.SafeSummary != summary {
		t.Fatalf("completed step = %#v", step)
	}
	steps, err := repo.ListDeploymentSteps(ctx, first.ID)
	if err != nil {
		t.Fatalf("ListDeploymentSteps() error = %v", err)
	}
	if len(steps) != 1 || steps[0].Name != "docker" || steps[0].Status != deployment.StepStatusCompleted {
		t.Fatalf("ListDeploymentSteps() = %#v", steps)
	}

	second := createTestDeployment(t, ctx, repo, "node-2", "192.0.2.11")
	if _, err := repo.UpdateDeploymentState(ctx, second.ID, repositorycontract.UpdateDeploymentStateParams{
		Status:      deployment.StatusCompleted,
		CurrentStep: "completed",
	}); err != nil {
		t.Fatalf("complete second deployment: %v", err)
	}

	recent, err := repo.ListRecentDeployments(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentDeployments() error = %v", err)
	}
	if len(recent) != 2 {
		t.Fatalf("recent deployment count = %d, want 2", len(recent))
	}

	unfinished, err := repo.FindUnfinishedDeployments(ctx, 10)
	if err != nil {
		t.Fatalf("FindUnfinishedDeployments() error = %v", err)
	}
	if len(unfinished) != 1 || unfinished[0].ID != first.ID {
		t.Fatalf("unfinished deployments = %#v, want only %s", unfinished, first.ID)
	}

	_, err = repo.GetDeployment(ctx, "11111111-1111-4111-8111-111111111111")
	if !errors.Is(err, repositorycontract.ErrNotFound) {
		t.Fatalf("GetDeployment(missing) error = %v, want ErrNotFound", err)
	}

	issuedAt := time.Now().Add(-time.Hour).UTC()
	expiresAt := time.Now().Add(90 * 24 * time.Hour).UTC()
	version := certmanager.Version{SNI: "de.example.com", Version: "v-20260807T120000Z-1234abcd", Fingerprint: strings.Repeat("a", 64), Serial: "1234", IssuedAt: issuedAt, ExpiresAt: expiresAt, Status: certmanager.VersionPending, CreatedAt: time.Now().UTC()}
	if err := repo.SaveVersion(ctx, version); err != nil {
		t.Fatalf("SaveVersion() error = %v", err)
	}
	if err := repo.ActivateVersion(ctx, version.SNI, version.Version, false); err != nil {
		t.Fatalf("ActivateVersion() error = %v", err)
	}
	certificate, err := repo.GetActive(ctx, version.SNI)
	if err != nil || certificate.ActiveVersion != version.Version || certificate.Fingerprint != version.Fingerprint {
		t.Fatalf("GetActive() = %#v, %v", certificate, err)
	}
	if err := repo.RecordDistribution(ctx, certmanager.DistributionRecord{SNI: version.SNI, Version: version.Version, DeploymentID: first.ID, NodeIP: first.TargetVPSIP, Status: certmanager.DistributionSucceeded, AttemptedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("RecordDistribution() error = %v", err)
	}
	versions, err := repo.ListVersions(ctx, version.SNI)
	if err != nil || len(versions) != 1 || versions[0].Status != certmanager.VersionActive {
		t.Fatalf("ListVersions() = %#v, %v", versions, err)
	}
}

func createTestDeployment(t *testing.T, ctx context.Context, repo *Repository, name, ip string) deployment.Deployment {
	t.Helper()
	result, err := repo.CreateDeployment(ctx, repositorycontract.CreateDeploymentParams{
		TelegramOperatorUserID:    123456,
		SelectedRemnawaveHostUUID: "a60d725f-17e9-4a50-9242-3dc223d5a0c9",
		SelectedHostRemark:        "Germany",
		SNIDomain:                 "de.example.com",
		NodeName:                  name,
		TargetVPSIP:               netip.MustParseAddr(ip),
	})
	if err != nil {
		t.Fatalf("CreateDeployment() error = %v", err)
	}
	return result
}

func assertNoRootPasswordColumn(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var count int
	err := pool.QueryRow(ctx, `
        SELECT count(*)
        FROM information_schema.columns
        WHERE table_schema = current_schema()
          AND table_name = 'deployments'
          AND column_name = 'root_password'`).Scan(&count)
	if err != nil {
		t.Fatalf("inspect deployments schema: %v", err)
	}
	if count != 0 {
		t.Fatal("deployments must not contain root_password")
	}
}
