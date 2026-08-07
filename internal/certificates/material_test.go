package certificates

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestMaterialFormattingAndLoggingAreRedacted(t *testing.T) {
	material := Material{FullchainPEM: []byte("certificate-secret"), PrivateKeyPEM: []byte("private-key-secret")}
	formatted := fmt.Sprintf("%v %#v", material, material)
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("material", slog.Any("certificate", material))
	combined := formatted + logs.String()
	for _, secret := range []string{"certificate-secret", "private-key-secret"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("formatted certificate material contains %q", secret)
		}
	}
}

func TestMaterialCloneAndDestroy(t *testing.T) {
	original := Material{FullchainPEM: []byte("certificate"), PrivateKeyPEM: []byte("private-key")}
	clone := original.Clone()
	clone.FullchainPEM[0] = 'C'
	if string(original.FullchainPEM) != "certificate" {
		t.Fatal("Clone() shares certificate storage")
	}
	clone.Destroy()
	if clone.FullchainPEM != nil || clone.PrivateKeyPEM != nil {
		t.Fatal("Destroy() retained material")
	}
}
