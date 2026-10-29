package orchestrator

import (
	"context"
	"reflect"
	"testing"
	"time"

	sshclient "remnanode-setup-bot/internal/ssh"
)

type netplanRunner struct{ output string }

func (r netplanRunner) Run(context.Context, sshclient.CommandRequest) (sshclient.CommandResult, error) {
	return sshclient.CommandResult{Stdout: r.output}, nil
}

func TestReadNetplanAddressesIncludesInterfacesAndExcludesNameservers(t *testing.T) {
	contents := `network:
  version: 2
  ethernets:
    eth0:
      addresses: [203.0.113.10/24, "2001:db8::10/64"]
      nameservers:
        addresses: [1.1.1.1, 8.8.8.8]
    eth1:
      addresses:
        - 198.51.100.20/32
`
	got, err := readNetplanAddresses(context.Background(), netplanRunner{output: contents}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"198.51.100.20", "2001:db8::10", "203.0.113.10"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("addresses = %#v, want %#v", got, want)
	}
}
