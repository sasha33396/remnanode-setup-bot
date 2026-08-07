package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

var ErrOutputLimitExceeded = errors.New("SSH command output limit exceeded")

// CommandRequest is deliberately not included in errors or logs. Stdin and
// Command may contain sensitive operational material.
type CommandRequest struct {
	Command string
	Stdin   []byte
	Timeout time.Duration
}

// CommandResult contains bounded output and an explicit remote exit status.
type CommandResult struct {
	Stdout     string
	Stderr     string
	ExitStatus int
	Duration   time.Duration
	TimedOut   bool
}

// CommandRunner allows preflight to be tested without a live SSH server.
type CommandRunner interface {
	Run(context.Context, CommandRequest) (CommandResult, error)
}

var _ CommandRunner = (*Connection)(nil)

// CommandError reports only exit status, never command text or output.
type CommandError struct {
	ExitStatus int
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("SSH command exited with status %d", e.ExitStatus)
}

// Run executes one command with cancellation, timeout, bounded stdout/stderr,
// and an explicit exit status.
func (c *Connection) Run(ctx context.Context, request CommandRequest) (CommandResult, error) {
	timeout := request.Timeout
	if timeout <= 0 {
		timeout = c.defaultTimeout
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	session, err := c.client.NewSession()
	if err != nil {
		return CommandResult{ExitStatus: -1}, fmt.Errorf("create SSH command session: %w", err)
	}
	defer session.Close()

	stdout := newLimitedBuffer(c.stdoutLimit)
	stderr := newLimitedBuffer(c.stderrLimit)
	session.Stdout = stdout
	session.Stderr = stderr
	if request.Stdin != nil {
		session.Stdin = bytes.NewReader(request.Stdin)
	}

	started := time.Now()
	if err := session.Start(request.Command); err != nil {
		return CommandResult{ExitStatus: -1, Duration: time.Since(started)}, fmt.Errorf("start SSH command: %w", err)
	}
	wait := make(chan error, 1)
	go func() { wait <- session.Wait() }()

	var waitErr error
	timedOut := false
	select {
	case waitErr = <-wait:
	case <-commandCtx.Done():
		timedOut = errors.Is(commandCtx.Err(), context.DeadlineExceeded)
		_ = session.Close()
		waitErr = <-wait
	}

	result := CommandResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ExitStatus: 0,
		Duration:   time.Since(started),
		TimedOut:   timedOut,
	}
	if commandCtx.Err() != nil {
		result.ExitStatus = -1
		return result, fmt.Errorf("SSH command cancelled: %w", commandCtx.Err())
	}
	var exitError *gossh.ExitError
	if waitErr == nil {
		result.ExitStatus = 0
	} else if errors.As(waitErr, &exitError) {
		result.ExitStatus = exitError.ExitStatus()
	} else {
		result.ExitStatus = -1
	}
	if stdout.exceeded || stderr.exceeded {
		return result, ErrOutputLimitExceeded
	}
	if waitErr == nil {
		return result, nil
	}
	if exitError != nil {
		return result, &CommandError{ExitStatus: result.ExitStatus}
	}
	return result, errors.New("SSH command failed without an exit status")
}

type limitedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	exceeded bool
}

func newLimitedBuffer(limit int) *limitedBuffer { return &limitedBuffer{limit: limit} }

func (b *limitedBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := b.limit - b.buffer.Len()
	if remaining < len(value) {
		b.exceeded = true
		if remaining < 0 {
			remaining = 0
		}
		value = value[:remaining]
	}
	_, _ = b.buffer.Write(value)
	return originalLength, nil
}

func (b *limitedBuffer) String() string { return b.buffer.String() }
