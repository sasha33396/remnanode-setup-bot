package postgres

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"remnanode-setup-bot/internal/deployment"
	repositorycontract "remnanode-setup-bot/internal/repository"
)

func TestValidUUID(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"a60d725f-17e9-4a50-9242-3dc223d5a0c9", true},
		{"A60D725F-17E9-4A50-9242-3DC223D5A0C9", true},
		{"not-a-uuid", false},
		{"a60d725f17e94a5092423dc223d5a0c9", false},
	} {
		if got := validUUID(test.value); got != test.valid {
			t.Errorf("validUUID(%q) = %t, want %t", test.value, got, test.valid)
		}
	}
}

func TestRepositoryRejectsInvalidInputBeforeQuery(t *testing.T) {
	repo := New(nil)
	ctx := context.Background()

	_, err := repo.CreateDeployment(ctx, repositorycontract.CreateDeploymentParams{
		ID:                        "invalid",
		TelegramOperatorUserID:    123,
		SelectedRemnawaveHostUUID: "a60d725f-17e9-4a50-9242-3dc223d5a0c9",
		SNIDomain:                 "de.example.com",
		NodeName:                  "node-1",
		TargetVPSIP:               netip.MustParseAddr("192.0.2.10"),
	})
	if !errors.Is(err, repositorycontract.ErrInvalidArgument) {
		t.Fatalf("CreateDeployment() error = %v, want ErrInvalidArgument", err)
	}

	_, err = repo.UpdateDeploymentState(ctx, "a60d725f-17e9-4a50-9242-3dc223d5a0c9", repositorycontract.UpdateDeploymentStateParams{
		Status:      deployment.Status("INVALID"),
		CurrentStep: "preflight",
	})
	if !errors.Is(err, repositorycontract.ErrInvalidArgument) {
		t.Fatalf("UpdateDeploymentState() error = %v, want ErrInvalidArgument", err)
	}

	_, err = repo.ListRecentDeployments(ctx, 0)
	if !errors.Is(err, repositorycontract.ErrInvalidArgument) {
		t.Fatalf("ListRecentDeployments() error = %v, want ErrInvalidArgument", err)
	}

	_, err = repo.SetNodeHostBinding(ctx, "a60d725f-17e9-4a50-9242-3dc223d5a0c9", repositorycontract.SetNodeHostBindingParams{
		HostUUID: "invalid", HostRemark: "target", SNIDomain: "new.example.com",
	})
	if !errors.Is(err, repositorycontract.ErrInvalidArgument) {
		t.Fatalf("SetNodeHostBinding() error = %v, want ErrInvalidArgument", err)
	}
}
