package ssh

import (
	"context"
	"errors"
	"strings"

	gossh "golang.org/x/crypto/ssh"
)

const installAuthorizedKeyCommand = `set -eu
if [ "$(id -u)" -ne 0 ]; then exit 77; fi
umask 077
mkdir -p /root/.ssh
touch /root/.ssh/authorized_keys
chmod 700 /root/.ssh
chmod 600 /root/.ssh/authorized_keys
key="$(cat)"
grep -qxF -- "$key" /root/.ssh/authorized_keys || printf '%s\n' "$key" >> /root/.ssh/authorized_keys`

// InstallDeploymentPublicKey installs this client's backend public key.
func (c *Client) InstallDeploymentPublicKey(ctx context.Context, connection *Connection) error {
	if connection == nil {
		return errors.New("SSH connection is required")
	}
	return connection.InstallDeploymentPublicKey(ctx, gossh.MarshalAuthorizedKey(c.deploymentKey.PublicKey()))
}

// InstallDeploymentPublicKey idempotently installs an externally supplied SSH
// public key. No private key or password is sent through a shell command.
func (c *Connection) InstallDeploymentPublicKey(ctx context.Context, authorizedKey []byte) error {
	publicKey, _, _, _, err := gossh.ParseAuthorizedKey(authorizedKey)
	if err != nil {
		return errors.New("invalid deployment SSH public key")
	}
	canonical := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(publicKey))) + "\n"
	_, err = c.Run(ctx, CommandRequest{Command: installAuthorizedKeyCommand, Stdin: []byte(canonical)})
	if err != nil {
		return errors.New("install deployment SSH public key failed")
	}
	return nil
}
