package provisioner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

type managedFile struct {
	path    string
	mode    string
	content []byte
}

type remoteStage struct {
	name            string
	runner          sshclient.CommandRunner
	timeout         time.Duration
	files           []managedFile
	inspectCommand  string
	applyCommand    string
	validateCommand string
}

func (s *remoteStage) Name() string { return s.name }

func (s *remoteStage) Inspect(ctx context.Context) (Inspection, error) {
	for _, file := range s.files {
		matches, err := s.fileMatches(ctx, file)
		if err != nil {
			return Inspection{}, err
		}
		if !matches {
			return Inspection{Summary: "managed configuration requires update"}, nil
		}
	}
	if s.inspectCommand != "" {
		ready, err := s.commandReady(ctx, s.inspectCommand)
		if err != nil {
			return Inspection{}, err
		}
		if !ready {
			return Inspection{Summary: "service state requires update"}, nil
		}
	}
	return Inspection{Satisfied: true, Summary: "already configured"}, nil
}

func (s *remoteStage) Apply(ctx context.Context) error {
	for _, file := range s.files {
		if err := s.writeFile(ctx, file); err != nil {
			return err
		}
	}
	if s.applyCommand != "" {
		return s.runChecked(ctx, s.applyCommand, nil)
	}
	return nil
}

func (s *remoteStage) Validate(ctx context.Context) error {
	for _, file := range s.files {
		matches, err := s.fileMatches(ctx, file)
		if err != nil || !matches {
			return errors.New("managed file validation failed")
		}
	}
	if s.validateCommand != "" {
		ready, err := s.commandReady(ctx, s.validateCommand)
		if err != nil || !ready {
			return errors.New("remote service validation failed")
		}
	}
	return nil
}

func (s *remoteStage) commandReady(ctx context.Context, command string) (bool, error) {
	result, err := s.runner.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: s.timeout})
	if err != nil {
		return false, errors.New("remote inspection command failed")
	}
	switch strings.TrimSpace(result.Stdout) {
	case "ready":
		return true, nil
	case "pending":
		return false, nil
	default:
		return false, errors.New("remote inspection returned an invalid response")
	}
}

func (s *remoteStage) runChecked(ctx context.Context, command string, stdin []byte) error {
	_, err := s.runner.Run(ctx, sshclient.CommandRequest{Command: command, Stdin: stdin, Timeout: s.timeout})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("remote apply command failed")
	}
	return nil
}

func (s *remoteStage) fileMatches(ctx context.Context, file managedFile) (bool, error) {
	path, err := shellPath(file.path)
	if err != nil {
		return false, err
	}
	command := fmt.Sprintf("if [ -f %s ]; then sha256sum %s | cut -d' ' -f1; else printf missing; fi", path, path)
	result, err := s.runner.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: s.timeout})
	if err != nil {
		return false, errors.New("managed file inspection failed")
	}
	want := sha256.Sum256(file.content)
	return strings.TrimSpace(result.Stdout) == hex.EncodeToString(want[:]), nil
}

func (s *remoteStage) writeFile(ctx context.Context, file managedFile) error {
	path, err := shellPath(file.path)
	if err != nil {
		return err
	}
	if !regexp.MustCompile(`^[0-7]{3,4}$`).MatchString(file.mode) {
		return errors.New("invalid managed file mode")
	}
	command := fmt.Sprintf(`set -eu
target=%s
directory=$(dirname "$target")
install -d -m 0755 "$directory"
temporary=$(mktemp "$directory/.provisioner.XXXXXX")
trap 'rm -f "$temporary"' EXIT
cat > "$temporary"
chmod %s "$temporary"
if [ -f "$target" ] && cmp -s "$temporary" "$target"; then
    exit 0
fi
mv -f "$temporary" "$target"
trap - EXIT`, path, file.mode)
	return s.runChecked(ctx, command, file.content)
}

func shellPath(value string) (string, error) {
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("managed file path must be an absolute path")
	}
	return shellQuote(value)
}

func shellQuote(value string) (string, error) {
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", errors.New("value cannot be represented safely in a shell command")
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'", nil
}
