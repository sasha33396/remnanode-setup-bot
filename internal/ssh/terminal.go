package ssh

import (
	"context"
	"errors"
	"io"
	"sync"

	gossh "golang.org/x/crypto/ssh"
)

// WindowSize is an SSH PTY size in character cells.
type WindowSize struct {
	Columns int
	Rows    int
}

// RunTerminal starts an interactive root shell over the verified connection.
// The caller owns cancellation and the input/output transports.
func (c *Connection) RunTerminal(ctx context.Context, input io.Reader, output io.Writer, initial WindowSize, resize <-chan WindowSize) error {
	if c == nil || c.client == nil || input == nil || output == nil || !validWindowSize(initial) {
		return ErrInvalidConfiguration
	}
	session, err := c.client.NewSession()
	if err != nil {
		return errors.New("create SSH terminal session failed")
	}
	defer session.Close()

	stdin, err := session.StdinPipe()
	if err != nil {
		return errors.New("create SSH terminal input failed")
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return errors.New("create SSH terminal output failed")
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return errors.New("create SSH terminal error output failed")
	}
	modes := gossh.TerminalModes{
		gossh.ECHO:          1,
		gossh.TTY_OP_ISPEED: 14400,
		gossh.TTY_OP_OSPEED: 14400,
	}
	if err := session.RequestPty("xterm-256color", initial.Rows, initial.Columns, modes); err != nil {
		return errors.New("request SSH terminal failed")
	}
	if err := session.Shell(); err != nil {
		return errors.New("start SSH terminal failed")
	}

	lockedOutput := &synchronizedWriter{writer: output}
	go func() { _, _ = io.Copy(stdin, input); _ = stdin.Close() }()
	go func() { _, _ = io.Copy(lockedOutput, stdout) }()
	go func() { _, _ = io.Copy(lockedOutput, stderr) }()
	wait := make(chan error, 1)
	go func() { wait <- session.Wait() }()

	for {
		select {
		case err := <-wait:
			return err
		case <-ctx.Done():
			_ = session.Close()
			<-wait
			return ctx.Err()
		case size, ok := <-resize:
			if !ok {
				resize = nil
				continue
			}
			if validWindowSize(size) {
				_ = session.WindowChange(size.Rows, size.Columns)
			}
		}
	}
}

func validWindowSize(size WindowSize) bool {
	return size.Columns >= 1 && size.Columns <= 1000 && size.Rows >= 1 && size.Rows <= 1000
}

type synchronizedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *synchronizedWriter) Write(value []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(value)
}
