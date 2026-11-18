package leastload

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"remnanode-setup-bot/internal/networkinventory"
	sshclient "remnanode-setup-bot/internal/ssh"
)

func run(ctx context.Context, r sshclient.CommandRunner, command string, input []byte) (string, error) {
	result, err := r.Run(ctx, sshclient.CommandRequest{Command: command, Stdin: input, Timeout: 20 * time.Second})
	return result.Stdout, err
}
func quote(s string) string     { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func digest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

// Admin rules permit preserving prefix/gateway for a new address. They are
// explicit CIDR pairs, never inferred from a provider name or a /32 address.
// Format: old-CIDR>new-CIDR;old-CIDR>new-CIDR
func allowedReplacement(oldIP, newIP netip.Addr) bool {
	for _, rule := range strings.Split(os.Getenv("LEASTLOAD_NETWORK_RULES"), ";") {
		from, to, ok := strings.Cut(strings.TrimSpace(rule), ">")
		if !ok {
			continue
		}
		a, e1 := netip.ParsePrefix(strings.TrimSpace(from))
		b, e2 := netip.ParsePrefix(strings.TrimSpace(to))
		if e1 == nil && e2 == nil && a.Contains(oldIP) && b.Contains(newIP) {
			return true
		}
	}
	return false
}

func Prepare(ctx context.Context, r sshclient.CommandRunner, oldIP, newIP netip.Addr) (Plan, error) {
	p := Plan{OldIP: oldIP.String(), NewIP: newIP.String()}
	report, err := networkinventory.Collect(ctx, r, oldIP)
	if err != nil {
		return p, Error("Не удалось прочитать сеть сервера")
	}
	p.Before = report
	p.Interface = report.NodeInterface
	for _, rule := range report.Rules {
		if (rule.Priority != 0 && rule.Priority != 32766 && rule.Priority != 32767) || (rule.From != "" && rule.From != "all") || (rule.To != "" && rule.To != "all") {
			return p, Error("Обнаружены нестандартные правила маршрутизации; автоматическая замена пока недоступна")
		}
	}
	if report.Status != "observed" || p.Interface == "" {
		return p, Error("Конфигурация сети неоднозначна; сначала проверьте сетевой отчёт")
	}
	var binding *networkinventory.Binding
	for _, file := range report.Files {
		if file.Kind != "Netplan" || !file.MatchesPrimary {
			continue
		}
		for _, b := range file.Bindings {
			if b.Interface == p.Interface {
				if binding != nil {
					return p, Error("Несколько исходных секций Netplan")
				}
				value := b
				binding = &value
				p.File = file.Path
				p.Section = b.ID
			}
		}
	}
	if binding == nil || binding.DHCP4 {
		return p, Error("Поддерживается только статический IPv4 в Netplan")
	}
	// Conservatively require one Netplan file; applying all files must not activate
	// unrelated staged edits in another configuration.
	count := 0
	for _, f := range report.Files {
		if f.Kind == "Netplan" {
			count++
		}
	}
	if count != 1 {
		return p, Error("Обнаружено несколько Netplan-файлов; автоматическая замена пока недоступна")
	}
	content, err := run(ctx, r, "cat -- "+quote(p.File), nil)
	if err != nil {
		return p, Error("Не удалось прочитать исходный Netplan")
	}
	p.FileHash = digest([]byte(content))
	updated, mode, prefix, err := rewrite([]byte(content), p.Section, oldIP, newIP)
	if err != nil {
		return p, err
	}
	p.Mode = mode
	p.Prefix = prefix
	p.TargetHash = digest(updated)
	liveNew := false
	for _, a := range report.Addresses {
		if a.IP == newIP.String() {
			if a.Interface != p.Interface || a.Prefix != prefix {
				return p, Error("Новый IP уже назначен другому интерфейсу или с другой маской")
			}
			liveNew = true
		}
	}
	if mode == "existing" && liveNew {
		p.Mode = "metadata"
	} else if mode == "existing" {
		p.Mode = "activate"
	} else if liveNew {
		return p, Error("Новый IP есть на интерфейсе, но отсутствует в Netplan")
	}
	if p.Mode == "replace" && !allowedReplacement(oldIP, newIP) {
		return p, Error("Для этой пары сетей администратор должен настроить LEASTLOAD_NETWORK_RULES; шлюз и маска не угадываются")
	}
	if p.Mode != "metadata" {
		if _, err := run(ctx, r, "command -v netplan >/dev/null && command -v systemd-run >/dev/null && command -v flock >/dev/null && command -v timeout >/dev/null && command -v sha256sum >/dev/null && systemctl is-active --quiet systemd-networkd", nil); err != nil {
			return p, Error("Недоступны средства применения Netplan или автономного отката")
		}
	}
	return p, nil
}

// Rewrite only the selected scalar, keeping every other YAML node (including
// unknown configuration) intact. Existing new IP means no file modification.
func rewrite(data []byte, section string, oldIP, newIP netip.Addr) ([]byte, string, int, error) {
	var root yaml.Node
	if yaml.Unmarshal(data, &root) != nil {
		return nil, "", 0, Error("Некорректный YAML Netplan")
	}
	var aliases bool
	var inspect func(*yaml.Node)
	inspect = func(n *yaml.Node) {
		if n.Kind == yaml.AliasNode || n.Anchor != "" {
			aliases = true
		}
		for _, c := range n.Content {
			inspect(c)
		}
	}
	inspect(&root)
	if aliases {
		return nil, "", 0, Error("Netplan с YAML-якорями требует ручной проверки")
	}
	get := func(n *yaml.Node, key string) *yaml.Node {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				return n.Content[i+1]
			}
		}
		return nil
	}
	if len(root.Content) != 1 {
		return nil, "", 0, Error("Ожидается один документ Netplan")
	}
	network := get(root.Content[0], "network")
	eth := get(network, "ethernets")
	entry := get(eth, section)
	addresses := get(entry, "addresses")
	if addresses == nil || addresses.Kind != yaml.SequenceNode {
		return nil, "", 0, Error("Не найдена статическая секция Ethernet")
	}
	var oldNode *yaml.Node
	oldCount, newCount, prefix, newPrefix := 0, 0, 0, 0
	for _, n := range addresses.Content {
		a, e := netip.ParsePrefix(n.Value)
		if e != nil {
			return nil, "", 0, Error("Неподдерживаемый формат адресов Netplan")
		}
		if a.Addr().Unmap() == oldIP {
			oldNode = n
			oldCount++
			prefix = a.Bits()
		}
		if a.Addr().Unmap() == newIP {
			newCount++
			newPrefix = a.Bits()
		}
	}
	if oldCount != 1 || newCount > 1 {
		return nil, "", 0, Error("Адреса в Netplan неоднозначны")
	}
	// Address occurrences outside the selected list (routing-policy, source
	// routes, other interfaces) cannot be blindly rewritten.
	occurrences := 0
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n == addresses {
			return
		}
		if n.Kind == yaml.ScalarNode {
			fields := strings.FieldsFunc(n.Value, func(r rune) bool { return r == '/' || r == ' ' || r == ',' })
			for _, f := range fields {
				if f == oldIP.String() || f == newIP.String() {
					occurrences++
				}
			}
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(&root)
	if occurrences > 0 {
		return nil, "", 0, Error("Адрес используется вне списка интерфейса; требуется проверка маршрутов и правил")
	}
	if newCount == 1 {
		return data, "existing", newPrefix, nil
	}
	oldNode.Value = fmt.Sprintf("%s/%d", newIP, prefix)
	out, err := yaml.Marshal(&root)
	return out, "replace", prefix, err
}
