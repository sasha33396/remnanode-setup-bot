package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

const defaultSSHPort = 22

// A pinned fingerprint identifies the key, but not its SSH algorithm. Some
// servers advertise a newer ED25519/ECDSA key before an older pinned RSA key.
// Try modern algorithms separately so a rejected key does not abort before the
// server can present the pinned one. Deliberately exclude SHA-1 ssh-rsa.
var pinnedHostKeyAlgorithmAttempts = [][]string{
	{gossh.CertAlgoED25519v01},
	{gossh.CertAlgoECDSA256v01},
	{gossh.CertAlgoECDSA384v01},
	{gossh.CertAlgoECDSA521v01},
	{gossh.CertAlgoRSASHA512v01},
	{gossh.CertAlgoRSASHA256v01},
	{gossh.KeyAlgoED25519},
	{gossh.KeyAlgoECDSA256},
	{gossh.KeyAlgoECDSA384},
	{gossh.KeyAlgoECDSA521},
	{gossh.KeyAlgoRSASHA512},
	{gossh.KeyAlgoRSASHA256},
}

var ErrInvalidConfiguration = errors.New("invalid SSH configuration")

// ParseDeploymentPrivateKey parses the backend key without including key data
// in returned errors.
func ParseDeploymentPrivateKey(privateKey []byte) (gossh.Signer, error) {
	signer, err := gossh.ParsePrivateKey(privateKey)
	if err != nil {
		return nil, errors.New("invalid deployment SSH private key")
	}
	return signer, nil
}

// Client creates password-authenticated initial connections and subsequent
// deployment-key connections.
type Client struct {
	hostKeys       HostKeyStore
	deploymentKey  gossh.Signer
	connectTimeout time.Duration
	commandTimeout time.Duration
	stdoutLimit    int
	stderrLimit    int
}

// NewClient requires a backend deployment key, TOFU store, explicit timeouts,
// and positive output limits.
func NewClient(hostKeys HostKeyStore, deploymentKey gossh.Signer, connectTimeout, commandTimeout time.Duration, stdoutLimit, stderrLimit int) (*Client, error) {
	if hostKeys == nil || deploymentKey == nil || connectTimeout <= 0 || commandTimeout <= 0 || stdoutLimit <= 0 || stderrLimit <= 0 {
		return nil, ErrInvalidConfiguration
	}
	return &Client{
		hostKeys:       hostKeys,
		deploymentKey:  deploymentKey,
		connectTimeout: connectTimeout,
		commandTimeout: commandTimeout,
		stdoutLimit:    stdoutLimit,
		stderrLimit:    stderrLimit,
	}, nil
}

// ConnectInitial authenticates with the in-memory temporary root password and
// clears the credentials' private password copy after this connection attempt.
func (c *Client) ConnectInitial(ctx context.Context, deploymentID string, credentials *InitialCredentials) (*Connection, error) {
	if credentials == nil || !credentials.Address.IsValid() || strings.TrimSpace(credentials.Username) == "" {
		return nil, ErrInvalidConfiguration
	}
	password := credentials.passwordString()
	if password == "" {
		return nil, ErrInvalidConfiguration
	}
	defer credentials.Clear()
	return c.connect(ctx, deploymentID, credentials.Address, credentials.Username, gossh.Password(password))
}

// ConnectWithDeploymentKey verifies the pinned fingerprint and authenticates
// with the backend deployment key.
func (c *Client) ConnectWithDeploymentKey(ctx context.Context, deploymentID string, address netip.Addr, username string) (*Connection, error) {
	if !address.IsValid() || strings.TrimSpace(username) == "" {
		return nil, ErrInvalidConfiguration
	}
	return c.connect(ctx, deploymentID, address, username, gossh.PublicKeys(c.deploymentKey))
}

func (c *Client) connect(ctx context.Context, deploymentID string, address netip.Addr, username string, auth gossh.AuthMethod) (*Connection, error) {
	if strings.TrimSpace(deploymentID) == "" {
		return nil, ErrInvalidConfiguration
	}
	connectCtx, cancel := context.WithTimeout(ctx, c.connectTimeout)
	defer cancel()

	_, pinned, err := c.hostKeys.Get(connectCtx, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("load trusted SSH host key: %w", err)
	}
	attempts := [][]string{nil}
	if pinned {
		attempts = pinnedHostKeyAlgorithmAttempts
	}

	var lastErr error
	for _, algorithms := range attempts {
		connection, hostKeyAccepted, err := c.connectAttempt(connectCtx, deploymentID, address, username, auth, algorithms)
		if err == nil {
			return connection, nil
		}
		lastErr = err
		if hostKeyAccepted {
			// The pinned server was reached. Retrying another host-key
			// algorithm cannot repair authentication or transport failures.
			return nil, err
		}
		if err := connectCtx.Err(); err != nil {
			return nil, fmt.Errorf("establish verified SSH connection: %w", err)
		}
	}
	return nil, lastErr
}

func (c *Client) connectAttempt(connectCtx context.Context, deploymentID string, address netip.Addr, username string, auth gossh.AuthMethod, hostKeyAlgorithms []string) (*Connection, bool, error) {
	verifier := newHostKeySession(connectCtx, deploymentID, c.hostKeys)
	config := &gossh.ClientConfig{
		User:              username,
		Auth:              []gossh.AuthMethod{auth},
		HostKeyCallback:   verifier.callback,
		HostKeyAlgorithms: hostKeyAlgorithms,
	}
	endpoint := net.JoinHostPort(address.String(), fmt.Sprint(defaultSSHPort))
	netConnection, err := (&net.Dialer{}).DialContext(connectCtx, "tcp", endpoint)
	if err != nil {
		return nil, false, fmt.Errorf("connect to SSH endpoint: %w", err)
	}
	defer func() {
		if netConnection != nil {
			_ = netConnection.Close()
		}
	}()
	if deadline, ok := connectCtx.Deadline(); ok {
		_ = netConnection.SetDeadline(deadline)
	}
	handshakeConnection := netConnection
	handshakeFinished := make(chan struct{})
	go func() {
		select {
		case <-connectCtx.Done():
			_ = handshakeConnection.Close()
		case <-handshakeFinished:
		}
	}()

	clientConnection, channels, requests, err := gossh.NewClientConn(netConnection, endpoint, config)
	close(handshakeFinished)
	if err != nil {
		return nil, verifier.accepted(), fmt.Errorf("establish verified SSH connection: %w", err)
	}
	if err := verifier.commit(); err != nil {
		_ = clientConnection.Close()
		return nil, verifier.accepted(), err
	}
	_ = netConnection.SetDeadline(time.Time{})
	sshClient := gossh.NewClient(clientConnection, channels, requests)
	netConnection = nil
	return &Connection{
		client:         sshClient,
		defaultTimeout: c.commandTimeout,
		stdoutLimit:    c.stdoutLimit,
		stderrLimit:    c.stderrLimit,
	}, true, nil
}

// Connection is an authenticated, host-key-verified SSH connection.
type Connection struct {
	client         *gossh.Client
	defaultTimeout time.Duration
	stdoutLimit    int
	stderrLimit    int
}

func (c *Connection) Close() error { return c.client.Close() }
