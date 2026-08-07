package remnawave

import (
	"errors"
	"net/netip"
	"testing"
)

func TestDeploymentProfileFromHost(t *testing.T) {
	profileUUID := "a60d725f-17e9-4a50-9242-3dc223d5a0c9"
	inboundUUID := "c55c6980-ac8e-4c6a-8579-dd3df8a29891"
	hostSNI := "must-not-be-used.example.com"

	profile, err := DeploymentProfileFromHost(Host{
		Address: "de.example.com",
		SNI:     &hostSNI,
		Inbound: HostInbound{
			ConfigProfileUUID:        &profileUUID,
			ConfigProfileInboundUUID: &inboundUUID,
		},
	})
	if err != nil {
		t.Fatalf("DeploymentProfileFromHost() error = %v", err)
	}
	if profile.SNIDomain != "de.example.com" {
		t.Errorf("SNI domain = %q, want Host address", profile.SNIDomain)
	}
	if profile.ActiveConfigProfileUUID != profileUUID {
		t.Errorf("active config profile = %q, want %q", profile.ActiveConfigProfileUUID, profileUUID)
	}
	if len(profile.ActiveInbounds) != 1 || profile.ActiveInbounds[0] != inboundUUID {
		t.Errorf("active inbounds = %v, want [%s]", profile.ActiveInbounds, inboundUUID)
	}
}

func TestDeploymentProfileFromHostRejectsMissingInboundProfile(t *testing.T) {
	profileUUID := "a60d725f-17e9-4a50-9242-3dc223d5a0c9"
	inboundUUID := "c55c6980-ac8e-4c6a-8579-dd3df8a29891"
	tests := []Host{
		{Address: "de.example.com", Inbound: HostInbound{ConfigProfileInboundUUID: &inboundUUID}},
		{Address: "de.example.com", Inbound: HostInbound{ConfigProfileUUID: &profileUUID}},
		{Inbound: HostInbound{ConfigProfileUUID: &profileUUID, ConfigProfileInboundUUID: &inboundUUID}},
	}
	for _, host := range tests {
		_, err := DeploymentProfileFromHost(host)
		if !errors.Is(err, ErrInvalidHostProfile) {
			t.Errorf("DeploymentProfileFromHost(%#v) error = %v, want ErrInvalidHostProfile", host, err)
		}
	}
}

func TestCheckNodeDuplicates(t *testing.T) {
	nodes := []Node{
		{Name: "node-1", Address: "192.0.2.10"},
		{Name: "node-2", Address: "2001:db8::1"},
	}

	tests := []struct {
		name          string
		candidateName string
		address       netip.Addr
		wantName      bool
		wantAddress   bool
	}{
		{"unique", "node-3", netip.MustParseAddr("192.0.2.30"), false, false},
		{"name", "node-1", netip.MustParseAddr("192.0.2.30"), true, false},
		{"address", "node-3", netip.MustParseAddr("2001:db8::1"), false, true},
		{"both", "node-1", netip.MustParseAddr("192.0.2.10"), true, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := CheckNodeDuplicates(nodes, test.candidateName, test.address)
			if !test.wantName && !test.wantAddress {
				if err != nil {
					t.Fatalf("CheckNodeDuplicates() error = %v", err)
				}
				return
			}
			if !errors.Is(err, ErrDuplicateNode) {
				t.Fatalf("CheckNodeDuplicates() error = %v, want ErrDuplicateNode", err)
			}
			var duplicate *DuplicateError
			if !errors.As(err, &duplicate) || duplicate.Name != test.wantName || duplicate.Address != test.wantAddress {
				t.Fatalf("duplicate = %#v, want name=%t address=%t", duplicate, test.wantName, test.wantAddress)
			}
		})
	}
}
