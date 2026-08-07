package deployment

import (
	"regexp"
	"testing"
)

func TestStatuses(t *testing.T) {
	terminal := []Status{StatusCompleted, StatusFailed, StatusCancelled, StatusDNSFailed}
	for _, status := range terminal {
		if !status.Valid() || !status.Terminal() {
			t.Errorf("status %q should be valid and terminal", status)
		}
	}
	if StatusProvisioning.Terminal() {
		t.Fatal("PROVISIONING must not be terminal")
	}
	if Status("UNKNOWN").Valid() {
		t.Fatal("UNKNOWN must not be valid")
	}
}

func TestStepStatuses(t *testing.T) {
	if !StepStatusRunning.Valid() || StepStatusRunning.Terminal() {
		t.Fatal("RUNNING should be valid and non-terminal")
	}
	if !StepStatusCompleted.Terminal() {
		t.Fatal("COMPLETED should be terminal")
	}
	if StepStatus("UNKNOWN").Valid() {
		t.Fatal("UNKNOWN must not be valid")
	}
}

func TestNewID(t *testing.T) {
	id, err := NewID()
	if err != nil {
		t.Fatalf("NewID() error = %v", err)
	}
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if !pattern.MatchString(id) {
		t.Fatalf("NewID() = %q, want RFC 4122 version 4 UUID", id)
	}
}
