package provisioner

import (
	"context"
	"errors"
)

var ErrXraySNIAdapterNotImplemented = errors.New("xray-sni installer adapter is not implemented")

// XraySNIInstaller keeps the stage engine independent from the installation
// mechanism. ExternalXraySNIInstaller is the production implementation; tests
// may inject a smaller fake without changing the engine.
type XraySNIInstaller interface {
	Inspect(context.Context) (Inspection, error)
	Install(context.Context) error
	Validate(context.Context) error
}

// NotImplementedXraySNIInstaller makes missing production integration explicit
// instead of silently installing the legacy ACME-based implementation.
type NotImplementedXraySNIInstaller struct{}

func (NotImplementedXraySNIInstaller) Inspect(context.Context) (Inspection, error) {
	return Inspection{}, ErrXraySNIAdapterNotImplemented
}
func (NotImplementedXraySNIInstaller) Install(context.Context) error {
	return ErrXraySNIAdapterNotImplemented
}
func (NotImplementedXraySNIInstaller) Validate(context.Context) error {
	return ErrXraySNIAdapterNotImplemented
}

type xraySNIStage struct{ installer XraySNIInstaller }

func (s xraySNIStage) Name() string { return "xray_sni" }
func (s xraySNIStage) Inspect(ctx context.Context) (Inspection, error) {
	return s.installer.Inspect(ctx)
}
func (s xraySNIStage) Apply(ctx context.Context) error    { return s.installer.Install(ctx) }
func (s xraySNIStage) Validate(ctx context.Context) error { return s.installer.Validate(ctx) }
