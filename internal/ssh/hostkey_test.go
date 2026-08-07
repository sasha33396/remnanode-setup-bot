package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func TestTOFUHostKeyFirstUseAndSubsequentVerification(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryHostKeyStore()
	firstKey := generatePublicKey(t)
	firstFingerprint := gossh.FingerprintSHA256(firstKey)

	first := newHostKeySession(ctx, "deployment-1", store)
	if err := first.callback("host", nil, firstKey); err != nil {
		t.Fatalf("first callback error = %v", err)
	}
	if _, found, _ := store.Get(ctx, "deployment-1"); found {
		t.Fatal("fingerprint was committed before successful authentication")
	}
	if err := first.commit(); err != nil {
		t.Fatalf("first commit error = %v", err)
	}
	trusted, found, err := store.Get(ctx, "deployment-1")
	if err != nil || !found || trusted != firstFingerprint {
		t.Fatalf("stored fingerprint = %q, %t, %v", trusted, found, err)
	}

	subsequent := newHostKeySession(ctx, "deployment-1", store)
	if err := subsequent.callback("host", nil, firstKey); err != nil {
		t.Fatalf("subsequent callback error = %v", err)
	}
}

func TestTOFUHostKeyRejectsChangedFingerprint(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryHostKeyStore()
	firstKey := generatePublicKey(t)
	first := newHostKeySession(ctx, "deployment-1", store)
	if err := first.callback("host", nil, firstKey); err != nil {
		t.Fatal(err)
	}
	if err := first.commit(); err != nil {
		t.Fatal(err)
	}

	changedKey := generatePublicKey(t)
	subsequent := newHostKeySession(ctx, "deployment-1", store)
	err := subsequent.callback("host", nil, changedKey)
	if !errors.Is(err, ErrHostKeyChanged) {
		t.Fatalf("changed callback error = %v, want ErrHostKeyChanged", err)
	}
	var changed *HostKeyChangedError
	if !errors.As(err, &changed) || changed.Expected != gossh.FingerprintSHA256(firstKey) || changed.Actual != gossh.FingerprintSHA256(changedKey) {
		t.Fatalf("HostKeyChangedError = %#v", changed)
	}
}

func TestTOFUDoesNotCommitFailedConnectionCandidate(t *testing.T) {
	store := NewMemoryHostKeyStore()
	session := newHostKeySession(context.Background(), "deployment-1", store)
	if err := session.callback("host", nil, generatePublicKey(t)); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.Get(context.Background(), "deployment-1"); found {
		t.Fatal("uncommitted host key candidate was stored")
	}
}

func generatePublicKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("create host signer: %v", err)
	}
	return signer.PublicKey()
}
