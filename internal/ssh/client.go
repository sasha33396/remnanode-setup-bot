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

	verifier := newHostKeySession(connectCtx, deploymentID, c.hostKeys)
	config := &gossh.ClientConfig{
		User:            username,
		Auth:            []gossh.AuthMethod{auth},
		HostKeyCallback: verifier.callback,
	}
	endpoint := net.JoinHostPort(address.String(), fmt.Sprint(defaultSSHPort))
	netConnection, err := (&net.Dialer{}).DialContext(connectCtx, "tcp", endpoint)
	if err != nil {
		return nil, fmt.Errorf("connect to SSH endpoint: %w", err)
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
		return nil, fmt.Errorf("establish verified SSH connection: %w", err)
	}
	if err := verifier.commit(); err != nil {
		_ = clientConnection.Close()
		return nil, err
	}
	_ = netConnection.SetDeadline(time.Time{})
	sshClient := gossh.NewClient(clientConnection, channels, requests)
	netConnection = nil
	return &Connection{
		client:         sshClient,
		defaultTimeout: c.commandTimeout,
		stdoutLimit:    c.stdoutLimit,
		stderrLimit:    c.stderrLimit,
	}, nil
}

// Connection is an authenticated, host-key-verified SSH connection.
type Connection struct {
	client         *gossh.Client
	defaultTimeout time.Duration
	stdoutLimit    int
	stderrLimit    int
}

func (c *Connection) Close() error { return c.client.Close() }
