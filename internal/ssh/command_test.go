package ssh

import (
	"errors"
	"strings"
	"testing"
)

func TestLimitedBufferBoundsOutput(t *testing.T) {
	buffer := newLimitedBuffer(4)
	written, err := buffer.Write([]byte("abcdef"))
	if err != nil || written != 6 {
		t.Fatalf("Write() = %d, %v", written, err)
	}
	if got := buffer.String(); got != "abcd" {
		t.Fatalf("bounded output = %q, want abcd", got)
	}
	if !buffer.exceeded {
		t.Fatal("buffer did not mark output limit exceeded")
	}
}

func TestCommandErrorsDoNotContainCommandOrSecret(t *testing.T) {
	secret := "temporary-root-password"
	command := "example --password " + secret
	commandErr := (&CommandError{ExitStatus: 17}).Error()
	outputErr := ErrOutputLimitExceeded.Error()
	for _, message := range []string{commandErr, outputErr} {
		if strings.Contains(message, command) || strings.Contains(message, secret) {
			t.Fatal("command error leaked command or secret")
		}
	}
	if !errors.Is(ErrOutputLimitExceeded, ErrOutputLimitExceeded) {
		t.Fatal("output limit sentinel is not comparable")
	}
}
