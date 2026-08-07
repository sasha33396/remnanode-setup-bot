package ssh

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func TestInitialCredentialsAreRedactedAndClearable(t *testing.T) {
	secret := []byte("temporary-root-password")
	credentials := NewInitialCredentials(netip.MustParseAddr("192.0.2.10"), "root", secret)
	for _, formatted := range []string{
		fmt.Sprint(credentials),
		fmt.Sprintf("%#v", credentials),
		fmt.Sprintf("%+v", *credentials),
		credentials.LogValue().String(),
	} {
		if strings.Contains(formatted, string(secret)) {
			t.Fatal("formatted credentials leaked temporary password")
		}
	}
	credentials.Clear()
	if got := credentials.passwordString(); got != "" {
		t.Fatal("Clear() did not remove the in-memory password")
	}
}
