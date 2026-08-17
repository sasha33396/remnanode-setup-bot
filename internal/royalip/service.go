// Package royalip replaces a Royal Hosting server's primary IPv4 address and
// default IPv4 gateway in netplan, then verifies SSH on the new address.
package royalip

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	sshclient "remnanode-setup-bot/internal/ssh"
)

var (
	ErrInvalidInput = errors.New("invalid Royal IP input")
	ErrSSH          = errors.New("Royal server SSH operation failed")
	ErrInterface    = errors.New("primary network interface was not found")
	ErrNetplan      = errors.New("compatible Royal netplan configuration was not found")
	ErrVerification = errors.New("new Royal IP could not be verified")
)

const (
	maxCommandTimeout   = 30 * time.Second
	maxOperationTimeout = 90 * time.Second
	reconnectInterval   = 2 * time.Second
	autoRollbackDelay   = 45 * time.Second
)

type Input struct {
	ServerIP netip.Addr
	NewIP    netip.Addr
	Password []byte
}

type Result struct {
	Interface   string
	PrefixBits  int
	Gateway     netip.Addr
	NetplanFile string
	BackupFile  string
}

type remoteSession interface {
	sshclient.CommandRunner
	Close() error
}

type passwordConnector interface {
	ConnectInitial(context.Context, string, *sshclient.InitialCredentials) (*sshclient.Connection, error)
}

type connectFunc func(context.Context, string, netip.Addr, []byte) (remoteSession, error)

type Service struct {
	connect connectFunc
	timeout time.Duration
}

func New(connector passwordConnector, timeout time.Duration) (*Service, error) {
	if connector == nil || timeout <= 0 {
		return nil, errors.New("invalid Royal IP service configuration")
	}
	if timeout > maxCommandTimeout {
		timeout = maxCommandTimeout
	}
	return &Service{
		timeout: timeout,
		connect: func(ctx context.Context, identity string, address netip.Addr, password []byte) (remoteSession, error) {
			credentials := sshclient.NewInitialCredentials(address, "root", password)
			defer credentials.Clear()
			return connector.ConnectInitial(ctx, identity, credentials)
		},
	}, nil
}

// Configure writes and validates netplan through the old address, applies it
// asynchronously because the SSH route will disappear, and verifies a fresh
// root SSH connection through the new address. Password is never persisted.
func (s *Service) Configure(ctx context.Context, input Input) (Result, error) {
	oldIP, newIP := input.ServerIP.Unmap(), input.NewIP.Unmap()
	if !oldIP.IsValid() || !oldIP.Is4() || !newIP.IsValid() || !newIP.Is4() || oldIP == newIP || len(input.Password) == 0 {
		return Result{}, ErrInvalidInput
	}
	operationCtx, cancel := context.WithTimeout(ctx, maxOperationTimeout)
	defer cancel()
	identity := "royal-server:" + oldIP.String()
	connection, err := s.connect(operationCtx, identity, oldIP, input.Password)
	if err != nil {
		return Result{}, ErrSSH
	}
	result, err := prepareNetplan(operationCtx, connection, oldIP, newIP, s.timeout)
	if err != nil {
		_ = connection.Close()
		return Result{}, err
	}
	marker := confirmationMarker(newIP)
	if _, err = run(operationCtx, connection, scheduledApplyCommand(result.NetplanFile, result.BackupFile, marker), nil, s.timeout); err != nil {
		rollbackPreparedNetplan(operationCtx, connection, result.NetplanFile, result.BackupFile, s.timeout)
		_ = connection.Close()
		return Result{}, ErrSSH
	}
	_ = connection.Close()

	for {
		if err := operationCtx.Err(); err != nil {
			return Result{}, ErrVerification
		}
		verified, connectErr := s.connect(operationCtx, identity, newIP, input.Password)
		if connectErr == nil {
			check, checkErr := run(operationCtx, verified, "ip -4 -o addr show dev "+result.Interface, nil, s.timeout)
			if checkErr == nil && strings.Contains(check.Stdout, newIP.String()+"/") {
				_, confirmErr := run(operationCtx, verified, "touch -- "+marker, nil, s.timeout)
				_ = verified.Close()
				if confirmErr == nil {
					return result, nil
				}
			} else {
				_ = verified.Close()
			}
		}
		timer := time.NewTimer(reconnectInterval)
		select {
		case <-operationCtx.Done():
			timer.Stop()
			return Result{}, ErrVerification
		case <-timer.C:
		}
	}
}

func confirmationMarker(newIP netip.Addr) string {
	return "/run/remnanode-royal-confirmed-" + strings.ReplaceAll(newIP.String(), ".", "-")
}

func scheduledApplyCommand(filename, backup, marker string) string {
	script := "sleep 2\n" +
		"if ! netplan apply; then cp -- " + shellQuote(backup) + " " + shellQuote(filename) + "; netplan apply; exit 1; fi\n" +
		"sleep " + fmt.Sprint(int(autoRollbackDelay/time.Second)) + "\n" +
		"if [ ! -f " + marker + " ]; then cp -- " + shellQuote(backup) + " " + shellQuote(filename) + "; netplan apply; fi"
	return "rm -f -- " + marker + " && nohup sh -c " + shellQuote(script) + " >/var/log/remnanode-royal-netplan.log 2>&1 </dev/null &"
}

func prepareNetplan(ctx context.Context, runner sshclient.CommandRunner, oldIP, newIP netip.Addr, timeout time.Duration) (Result, error) {
	addresses, err := run(ctx, runner, "ip -4 -o addr show", nil, timeout)
	if err != nil {
		return Result{}, ErrSSH
	}
	iface := interfaceForIP(addresses.Stdout, oldIP.String())
	if iface == "" {
		return Result{}, ErrInterface
	}
	listed, err := run(ctx, runner, "find /etc/netplan -maxdepth 1 -type f \\( -name '*.yaml' -o -name '*.yml' \\) -print", nil, timeout)
	if err != nil {
		return Result{}, ErrSSH
	}
	files := strings.Fields(listed.Stdout)
	sort.Strings(files)
	for _, filename := range files {
		if !safeNetplanPath(filename) {
			continue
		}
		read, readErr := run(ctx, runner, "cat -- "+shellQuote(filename), nil, timeout)
		if readErr != nil {
			continue
		}
		updated, key, prefixBits, gateway, updateErr := replaceIPv4AndGateway([]byte(read.Stdout), iface, oldIP, newIP)
		if key == "" {
			continue
		}
		if updateErr != nil {
			return Result{}, ErrNetplan
		}
		backup := filename + ".bak-royalbot"
		if _, copyErr := run(ctx, runner, "cp -- "+shellQuote(filename)+" "+shellQuote(backup), nil, timeout); copyErr != nil {
			return Result{}, ErrSSH
		}
		if _, writeErr := run(ctx, runner, "cat > "+shellQuote(filename), updated, timeout); writeErr != nil {
			rollbackPreparedNetplan(ctx, runner, filename, backup, timeout)
			return Result{}, ErrSSH
		}
		if _, generateErr := run(ctx, runner, "netplan generate", nil, timeout); generateErr != nil {
			rollbackPreparedNetplan(ctx, runner, filename, backup, timeout)
			return Result{}, ErrNetplan
		}
		_, _ = run(ctx, runner, "test -f /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg || printf '%s\\n' 'network: {config: disabled}' > /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg", nil, timeout)
		return Result{Interface: iface, PrefixBits: prefixBits, Gateway: gateway, NetplanFile: filename, BackupFile: backup}, nil
	}
	return Result{}, ErrNetplan
}

func rollbackPreparedNetplan(ctx context.Context, runner sshclient.CommandRunner, filename, backup string, timeout time.Duration) {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	_, _ = run(rollbackCtx, runner, "cp -- "+shellQuote(backup)+" "+shellQuote(filename), nil, timeout)
	_, _ = run(rollbackCtx, runner, "netplan generate", nil, timeout)
}

func replaceIPv4AndGateway(document []byte, iface string, oldIP, newIP netip.Addr) ([]byte, string, int, netip.Addr, error) {
	var root map[string]any
	if err := yaml.Unmarshal(document, &root); err != nil {
		return nil, "", 0, netip.Addr{}, err
	}
	network, ok := stringMap(root["network"])
	if !ok {
		return nil, "", 0, netip.Addr{}, errors.New("network section is missing")
	}
	ethernets, ok := stringMap(network["ethernets"])
	if !ok {
		return nil, "", 0, netip.Addr{}, errors.New("ethernets section is missing")
	}
	for key, raw := range ethernets {
		entry, entryOK := stringMap(raw)
		if !entryOK || !matchesInterface(key, entry, iface) {
			continue
		}
		addresses, addressErr := stringList(entry["addresses"])
		if addressErr != nil {
			return nil, key, 0, netip.Addr{}, addressErr
		}
		prefixBits := 0
		for index, value := range addresses {
			prefix, parseErr := netip.ParsePrefix(strings.TrimSpace(value))
			if parseErr == nil && prefix.Addr().Unmap() == oldIP {
				prefixBits = prefix.Bits()
				addresses[index] = fmt.Sprintf("%s/%d", newIP, prefixBits)
				break
			}
		}
		if prefixBits == 0 {
			return nil, key, 0, netip.Addr{}, errors.New("current IPv4 address is missing from netplan")
		}
		gateway := gatewayFor(newIP)
		routes, routesErr := mapList(entry["routes"])
		if routesErr != nil {
			return nil, key, 0, netip.Addr{}, routesErr
		}
		gatewayChanged := false
		for _, route := range routes {
			to, _ := route["to"].(string)
			via, _ := route["via"].(string)
			viaIP, parseErr := netip.ParseAddr(strings.TrimSpace(via))
			if (strings.TrimSpace(to) == "default" || strings.TrimSpace(to) == "0.0.0.0/0") && parseErr == nil && viaIP.Unmap().Is4() {
				route["via"] = gateway.String()
				gatewayChanged = true
			}
		}
		if !gatewayChanged {
			return nil, key, 0, netip.Addr{}, errors.New("default IPv4 route is missing from netplan")
		}
		entry["addresses"] = addresses
		entry["routes"] = mapsToAny(routes)
		updated, marshalErr := yaml.Marshal(root)
		return updated, key, prefixBits, gateway, marshalErr
	}
	return nil, "", 0, netip.Addr{}, nil
}

func gatewayFor(address netip.Addr) netip.Addr {
	bytes := address.Unmap().As4()
	bytes[3] = 1
	return netip.AddrFrom4(bytes)
}

func matchesInterface(key string, entry map[string]any, iface string) bool {
	if key == iface {
		return true
	}
	setName, _ := entry["set-name"].(string)
	return strings.TrimSpace(setName) == iface
}

func stringMap(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}

func stringList(value any) ([]string, error) {
	typed, ok := value.([]any)
	if !ok {
		if direct, directOK := value.([]string); directOK {
			return append([]string(nil), direct...), nil
		}
		return nil, errors.New("netplan value must be a list")
	}
	result := make([]string, 0, len(typed))
	for _, item := range typed {
		text, itemOK := item.(string)
		if !itemOK {
			return nil, errors.New("netplan list values must be strings")
		}
		result = append(result, text)
	}
	return result, nil
}

func mapList(value any) ([]map[string]any, error) {
	typed, ok := value.([]any)
	if !ok {
		return nil, errors.New("netplan routes must be a list")
	}
	result := make([]map[string]any, 0, len(typed))
	for _, item := range typed {
		entry, itemOK := item.(map[string]any)
		if !itemOK {
			return nil, errors.New("netplan routes must contain mappings")
		}
		result = append(result, entry)
	}
	return result, nil
}

func mapsToAny(values []map[string]any) []any {
	result := make([]any, len(values))
	for index := range values {
		result[index] = values[index]
	}
	return result
}

var interfaceName = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,64}$`)

func interfaceForIP(output, address string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[2] != "inet" || !strings.HasPrefix(fields[3], address+"/") {
			continue
		}
		iface := strings.SplitN(fields[1], "@", 2)[0]
		if interfaceName.MatchString(iface) {
			return iface
		}
	}
	return ""
}

func safeNetplanPath(value string) bool {
	if !strings.HasPrefix(value, "/etc/netplan/") || path.Clean(value) != value {
		return false
	}
	base := path.Base(value)
	if len(base) == 0 || len(base) > 128 || (!strings.HasSuffix(base, ".yaml") && !strings.HasSuffix(base, ".yml")) {
		return false
	}
	for _, character := range base {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || strings.ContainsRune("._-", character) {
			continue
		}
		return false
	}
	return true
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func run(ctx context.Context, runner sshclient.CommandRunner, command string, stdin []byte, timeout time.Duration) (sshclient.CommandResult, error) {
	return runner.Run(ctx, sshclient.CommandRequest{Command: command, Stdin: stdin, Timeout: timeout})
}
