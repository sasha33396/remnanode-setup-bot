package certificates

import "log/slog"

// Material is certificate data supplied by a centralized certificate source.
// It is intentionally transport-neutral and must never be logged or persisted.
type Material struct {
	FullchainPEM  []byte
	PrivateKeyPEM []byte
}

// Clone prevents the adapter and its caller from sharing mutable secret bytes.
func (m Material) Clone() Material {
	return Material{
		FullchainPEM:  append([]byte(nil), m.FullchainPEM...),
		PrivateKeyPEM: append([]byte(nil), m.PrivateKeyPEM...),
	}
}

// Destroy clears the material copy retained by its owner.
func (m *Material) Destroy() {
	clear(m.FullchainPEM)
	clear(m.PrivateKeyPEM)
	m.FullchainPEM = nil
	m.PrivateKeyPEM = nil
}

func (Material) String() string   { return "CertificateMaterial{[REDACTED]}" }
func (Material) GoString() string { return "CertificateMaterial{[REDACTED]}" }

// LogValue prevents slog from reflecting over PEM byte slices.
func (Material) LogValue() slog.Value {
	return slog.StringValue("CertificateMaterial{[REDACTED]}")
}
