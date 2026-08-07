package certmanager

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"remnanode-setup-bot/internal/certificates"
)

const (
	fullchainName  = "fullchain.pem"
	privateKeyName = "privkey.pem"
	currentName    = "current"
)

var versionPattern = regexp.MustCompile(`^v-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)

// FileStore persists protected, immutable certificate versions. The current
// marker is a small atomically replaced file rather than a symlink so backup
// and restore semantics are identical on Linux and Windows.
type FileStore struct {
	root string
}

func NewFileStore(root string) (*FileStore, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, ErrInvalidInput
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, safe("Certificate store path is invalid", ErrStorageFailed)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, safe("Certificate store is unavailable", ErrStorageFailed)
	}
	if err := os.Chmod(absolute, 0o700); err != nil {
		return nil, safe("Certificate store permissions could not be protected", ErrStorageFailed)
	}
	return &FileStore{root: absolute}, nil
}

func (s *FileStore) Stage(ctx context.Context, sni string, material certificates.Material) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	domain, err := canonicalSNI(sni)
	if err != nil {
		return "", err
	}
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", safe("Certificate version could not be generated", ErrStorageFailed)
	}
	version := "v-" + utcNow().Format("20060102T150405Z") + "-" + hex.EncodeToString(suffix[:])
	domainDir := filepath.Join(s.root, domain)
	if err := os.MkdirAll(domainDir, 0o700); err != nil {
		return "", safe("Certificate domain storage is unavailable", ErrStorageFailed)
	}
	if err := os.Chmod(domainDir, 0o700); err != nil {
		return "", safe("Certificate domain storage is not protected", ErrStorageFailed)
	}
	temporary, err := os.MkdirTemp(domainDir, ".stage-")
	if err != nil {
		return "", safe("Certificate version could not be staged", ErrStorageFailed)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(temporary)
		}
	}()
	if err := os.Chmod(temporary, 0o700); err != nil {
		return "", safe("Certificate staging permissions failed", ErrStorageFailed)
	}
	if err := writeExclusive(filepath.Join(temporary, fullchainName), material.FullchainPEM, 0o644); err != nil {
		return "", err
	}
	if err := writeExclusive(filepath.Join(temporary, privateKeyName), material.PrivateKeyPEM, 0o600); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	final := filepath.Join(domainDir, version)
	if err := os.Rename(temporary, final); err != nil {
		return "", safe("Certificate version could not be committed", ErrStorageFailed)
	}
	committed = true
	return version, nil
}

func (s *FileStore) Load(ctx context.Context, sni, version string) (certificates.Material, error) {
	if err := ctx.Err(); err != nil {
		return certificates.Material{}, err
	}
	domain, err := canonicalSNI(sni)
	if err != nil || !versionPattern.MatchString(version) {
		return certificates.Material{}, ErrInvalidInput
	}
	directory := filepath.Join(s.root, domain, version)
	fullchainPath := filepath.Join(directory, fullchainName)
	privateKeyPath := filepath.Join(directory, privateKeyName)
	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(privateKeyPath)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return certificates.Material{}, safe("Certificate private key permissions are unsafe", ErrStorageFailed)
		}
	}
	fullchain, err := os.ReadFile(fullchainPath)
	if err != nil {
		return certificates.Material{}, safe("Certificate version is unavailable", ErrStorageFailed)
	}
	privateKey, err := os.ReadFile(privateKeyPath)
	if err != nil {
		clear(fullchain)
		return certificates.Material{}, safe("Certificate version is unavailable", ErrStorageFailed)
	}
	return certificates.Material{FullchainPEM: fullchain, PrivateKeyPEM: privateKey}, nil
}

func (s *FileStore) ActiveVersion(ctx context.Context, sni string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	domain, err := canonicalSNI(sni)
	if err != nil {
		return "", err
	}
	value, err := os.ReadFile(filepath.Join(s.root, domain, currentName))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", safe("Active certificate marker is unavailable", ErrStorageFailed)
	}
	version := strings.TrimSpace(string(value))
	clear(value)
	if !versionPattern.MatchString(version) {
		return "", safe("Active certificate marker is invalid", ErrStorageFailed)
	}
	return version, nil
}

func (s *FileStore) Activate(ctx context.Context, sni, version string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	domain, err := canonicalSNI(sni)
	if err != nil || !versionPattern.MatchString(version) {
		return ErrInvalidInput
	}
	domainDir := filepath.Join(s.root, domain)
	if info, err := os.Stat(filepath.Join(domainDir, version)); err != nil || !info.IsDir() {
		return safe("Certificate version cannot be activated", ErrStorageFailed)
	}
	temporary, err := os.CreateTemp(domainDir, ".current-")
	if err != nil {
		return safe("Active certificate marker could not be staged", ErrStorageFailed)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return safe("Active certificate marker permissions failed", ErrStorageFailed)
	}
	if _, err := temporary.WriteString(version + "\n"); err != nil {
		_ = temporary.Close()
		return safe("Active certificate marker could not be written", ErrStorageFailed)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return safe("Active certificate marker could not be synchronized", ErrStorageFailed)
	}
	if err := temporary.Close(); err != nil {
		return safe("Active certificate marker could not be closed", ErrStorageFailed)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := replaceFile(temporaryName, filepath.Join(domainDir, currentName)); err != nil {
		return safe("Active certificate marker could not be replaced", ErrStorageFailed)
	}
	return nil
}

func writeExclusive(path string, value []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return safe("Certificate version file could not be created", ErrStorageFailed)
	}
	if _, err := file.Write(value); err != nil {
		_ = file.Close()
		return safe("Certificate version file could not be written", ErrStorageFailed)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return safe("Certificate version file could not be synchronized", ErrStorageFailed)
	}
	if err := file.Close(); err != nil {
		return safe("Certificate version file could not be closed", ErrStorageFailed)
	}
	return nil
}

func replaceFile(source, target string) error {
	if err := os.Rename(source, target); err == nil {
		return nil
	}
	// Windows does not replace an existing destination. The fallback is used by
	// tests and local development; production Linux uses atomic rename above.
	backup := target + ".previous"
	_ = os.Remove(backup)
	if err := os.Rename(target, backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(source, target); err != nil {
		_ = os.Rename(backup, target)
		return err
	}
	_ = os.Remove(backup)
	return nil
}

func canonicalSNI(value string) (string, error) {
	value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if len(value) < 3 || len(value) > 253 || !strings.Contains(value, ".") || strings.ContainsAny(value, "/\\\x00\r\n") {
		return "", ErrInvalidInput
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", ErrInvalidInput
		}
		for _, character := range label {
			if !(character >= 'a' && character <= 'z') && !(character >= '0' && character <= '9') && character != '-' {
				return "", ErrInvalidInput
			}
		}
	}
	return value, nil
}

var utcNow = func() time.Time { return time.Now().UTC() }

var _ Store = (*FileStore)(nil)
