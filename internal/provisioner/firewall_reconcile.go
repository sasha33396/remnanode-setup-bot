package provisioner

import (
	"context"
	"errors"
	"net/netip"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

// ReconcileFirewall reapplies the same idempotent firewall stage used during
// provisioning and validates both UFW and the full nftables port 25 guard.
func ReconcileFirewall(ctx context.Context, runner sshclient.CommandRunner, panelIP, metricsIP netip.Addr, timeout time.Duration) (bool, error) {
	if runner == nil || !panelIP.IsValid() || !metricsIP.IsValid() || timeout <= 0 {
		return false, errors.New("invalid firewall reconciliation configuration")
	}
	inspect, apply, validate := firewallCommands(panelIP.Unmap().String(), metricsIP.Unmap().String())
	stage := &remoteStage{name: "firewall", runner: runner, timeout: timeout, inspectCommand: inspect, applyCommand: apply, validateCommand: validate}
	state, err := stage.Inspect(ctx)
	if err != nil {
		return false, err
	}
	if state.Satisfied {
		return false, stage.Validate(ctx)
	}
	if err := stage.Apply(ctx); err != nil {
		return false, err
	}
	return true, stage.Validate(ctx)
}
