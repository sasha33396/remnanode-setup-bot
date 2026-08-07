package provisioner

import (
	"errors"
	"time"

	"remnanode-setup-bot/internal/certificates"
)

var ErrInvalidCertificateMaterial = errors.New("certificate material is invalid")

func validateCertificateMaterial(material certificates.Material, hostname string, now time.Time) error {
	if _, err := certificates.Validate(material, hostname, now); err != nil {
		return ErrInvalidCertificateMaterial
	}
	return nil
}
