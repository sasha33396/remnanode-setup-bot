package metrics_test

import (
	"testing"

	"remnanode-setup-bot/internal/metrics"
	"remnanode-setup-bot/internal/orchestrator"
)

func TestObservedRemnawaveImplementsProfileHostAPI(t *testing.T) {
	var api orchestrator.ProfileHostRemnawaveAPI = (*metrics.RemnawaveClient)(nil)
	if api == nil {
		t.Fatal("observed Remnawave client has no Profile Host API")
	}
}
