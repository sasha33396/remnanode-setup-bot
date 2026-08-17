// Package cherryip configures a Cherry Servers floating IPv4 address on a VPS.
// It intentionally does not call the Cherry API: the address must already be
// assigned to the server in Cherry. The service only performs the operating
// system step (live address plus persistent netplan configuration).
package cherryip

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
	ErrInvalidInput = errors.New("invalid Cherry IP input")
	ErrSSH          = errors.New("Cherry server SSH operation failed")
	ErrInterface    = errors.New("primary network interface was not found")
)

type Input struct {
	ServerIP   netip.Addr
	FloatingIP netip.Addr
	Password   []byte
}

type Result struct {
	Interface      string
	LiveConfigured bool
	Persistent     bool
	PersistentNote string
}

type passwordConnector interface {
	ConnectInitial(context.Context, string, *sshclient.InitialCredentials) (*sshclient.Connection, error)
}

type Service struct {
	ssh     passwordConnector
	timeout time.Duration
}

func New(ssh passwordConnector, timeout time.Duration) (*Service, error) {
	if ssh == nil || timeout <= 0 {
		return nil, errors.New("invalid Cherry IP service configuration")
	}
	return &Service{ssh: ssh, timeout: timeout}, nil
}

// Configure connects with the transient root password. The password is never
// persisted and the caller remains responsible for clearing its own copy.
func (s *Service) Configure(ctx context.Context, input Input) (Result, error) {
	serverIP, floatingIP := input.ServerIP.Unmap(), input.FloatingIP.Unmap()
	if !serverIP.IsValid() || !serverIP.Is4() || !floatingIP.IsValid() || !floatingIP.Is4() || serverIP == floatingIP || len(input.Password) == 0 {
		return Result{}, ErrInvalidInput
	}
	credentials := sshclient.NewInitialCredentials(serverIP, "root", input.Password)
	defer credentials.Clear()
	connection, err := s.ssh.ConnectInitial(ctx, "cherry-server:"+serverIP.String(), credentials)
	if err != nil {
		return Result{}, ErrSSH
	}
	defer connection.Close()
	return configure(ctx, connection, serverIP, floatingIP, s.timeout)
}

func configure(ctx context.Context, runner sshclient.CommandRunner, serverIP, floatingIP netip.Addr, timeout time.Duration) (Result, error) {
	if runner == nil || !serverIP.Is4() || !floatingIP.Is4() || timeout <= 0 {
		return Result{}, ErrInvalidInput
	}
	addresses, err := run(ctx, runner, "ip -4 -o addr show", nil, timeout)
	if err != nil {
		return Result{}, ErrSSH
	}
	iface := interfaceForIP(addresses.Stdout, serverIP.String())
	if iface == "" {
		return Result{}, ErrInterface
	}
	addCommand := fmt.Sprintf("ip addr add %s/32 dev %s", floatingIP, iface)
	_, _ = run(ctx, runner, addCommand, nil, timeout) // EEXIST is harmless; verification below is authoritative.
	check, err := run(ctx, runner, "ip -4 -o addr show dev "+iface, nil, timeout)
	if err != nil || !strings.Contains(check.Stdout, floatingIP.String()+"/32") {
		return Result{}, ErrSSH
	}

	persistent, note := persistNetplan(ctx, runner, iface, floatingIP, timeout)
	return Result{Interface: iface, LiveConfigured: true, Persistent: persistent, PersistentNote: note}, nil
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

func persistNetplan(ctx context.Context, runner sshclient.CommandRunner, iface string, floatingIP netip.Addr, timeout time.Duration) (bool, string) {
	listed, err := run(ctx, runner, "find /etc/netplan -maxdepth 1 -type f \\( -name '*.yaml' -o -name '*.yml' \\) -print", nil, timeout)
	if err != nil {
		return false, "не удалось получить список netplan-конфигов"
	}
	files := strings.Fields(listed.Stdout)
	sort.Strings(files)
	seen := make([]string, 0)
	for _, filename := range files {
		if !safeNetplanPath(filename) {
			continue
		}
		read, readErr := run(ctx, runner, "cat -- "+shellQuote(filename), nil, timeout)
		if readErr != nil {
			continue
		}
		updated, key, changed, parseErr := addAddressToNetplan([]byte(read.Stdout), iface, floatingIP.String()+"/32")
		if key != "" {
			seen = append(seen, path.Base(filename)+":"+key)
		}
		if parseErr != nil || key == "" {
			continue
		}
		if !changed {
			return true, "адрес уже присутствует в " + filename
		}
		backup := filename + ".bak-cherrybot"
		if _, copyErr := run(ctx, runner, "cp -- "+shellQuote(filename)+" "+shellQuote(backup), nil, timeout); copyErr != nil {
			return false, "не удалось создать резервную копию " + filename
		}
		if _, writeErr := run(ctx, runner, "cat > "+shellQuote(filename), updated, timeout); writeErr != nil {
			rollbackNetplan(ctx, runner, filename, backup, timeout)
			return false, "не удалось записать " + filename + "; исходный файл восстановлен"
		}
		if _, generateErr := run(ctx, runner, "netplan generate", nil, timeout); generateErr != nil {
			rollbackNetplan(ctx, runner, filename, backup, timeout)
			return false, "netplan не принял конфигурацию; исходный файл восстановлен"
		}
		_, _ = run(ctx, runner, "test -f /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg || printf '%s\\n' 'network: {config: disabled}' > /etc/cloud/cloud.cfg.d/99-disable-network-config.cfg", nil, timeout)
		if _, applyErr := run(ctx, runner, "netplan apply", nil, timeout); applyErr != nil {
			rollbackNetplan(ctx, runner, filename, backup, timeout)
			return false, "netplan apply завершился ошибкой; исходный файл восстановлен"
		}
		return true, "добавлено в " + filename + "; резервная копия: " + backup
	}
	if len(files) == 0 {
		return false, "netplan-конфиг в /etc/netplan не найден"
	}
	if len(seen) > 0 {
		return false, "интерфейс " + iface + " не найден в подходящей netplan-записи"
	}
	return false, "подходящий netplan-конфиг не найден"
}

func rollbackNetplan(ctx context.Context, runner sshclient.CommandRunner, filename, backup string, timeout time.Duration) {
	_, _ = run(context.WithoutCancel(ctx), runner, "cp -- "+shellQuote(backup)+" "+shellQuote(filename), nil, timeout)
	_, _ = run(context.WithoutCancel(ctx), runner, "netplan apply", nil, timeout)
}

func addAddressToNetplan(document []byte, iface, address string) ([]byte, string, bool, error) {
	var root map[string]any
	if err := yaml.Unmarshal(document, &root); err != nil {
		return nil, "", false, err
	}
	network, ok := stringMap(root["network"])
	if !ok {
		return nil, "", false, errors.New("network section is missing")
	}
	ethernets, ok := stringMap(network["ethernets"])
	if !ok {
		return nil, "", false, errors.New("ethernets section is missing")
	}
	for key, raw := range ethernets {
		entry, entryOK := stringMap(raw)
		if !entryOK {
			continue
		}
		if key != iface {
			setName, _ := entry["set-name"].(string)
			if strings.TrimSpace(setName) != iface {
				continue
			}
		}
		addresses, addressErr := addressList(entry["addresses"])
		if addressErr != nil {
			return nil, key, false, addressErr
		}
		for _, existing := range addresses {
			if existing == address {
				return document, key, false, nil
			}
		}
		entry["addresses"] = append(addresses, address)
		updated, err := yaml.Marshal(root)
		return updated, key, err == nil, err
	}
	return nil, "", false, nil
}

func stringMap(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}

func addressList(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	switch typed := value.(type) {
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, errors.New("netplan addresses must be strings")
			}
			result = append(result, text)
		}
		return result, nil
	case []string:
		return append([]string(nil), typed...), nil
	default:
		return nil, errors.New("netplan addresses must be a list")
	}
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
