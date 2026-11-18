// Package networkinventory collects read-only evidence, never applies network changes.
package networkinventory

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"path"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	sshclient "remnanode-setup-bot/internal/ssh"
)

type Address struct {
	Interface string `json:"interface"`
	IP        string `json:"ip"`
	Prefix    int    `json:"prefix"`
	Primary   bool   `json:"primary"`
}
type Route struct {
	Destination string `json:"dst"`
	Gateway     string `json:"gateway,omitempty"`
	Interface   string `json:"dev,omitempty"`
	Table       any    `json:"table,omitempty"`
	Source      string `json:"prefsrc,omitempty"`
}
type Rule struct {
	Priority int    `json:"priority"`
	From     string `json:"src,omitempty"`
	To       string `json:"dst,omitempty"`
	Table    any    `json:"table,omitempty"`
}
type ConfigFile struct {
	Path           string `json:"path"`
	Kind           string `json:"kind"`
	MatchesPrimary bool   `json:"matchesPrimary"`
}
type Report struct {
	PanelID     string       `json:"panelId"`
	NodeUUID    string       `json:"nodeUuid"`
	NodeName    string       `json:"nodeName"`
	PrimaryIP   string       `json:"primaryIp"`
	CollectedAt time.Time    `json:"collectedAt"`
	Status      string       `json:"status"`
	Addresses   []Address    `json:"addresses"`
	Routes      []Route      `json:"routes"`
	Rules       []Rule       `json:"rules"`
	Files       []ConfigFile `json:"files"`
	Services    []string     `json:"services"`
	Warnings    []string     `json:"warnings"`
}
type Store interface {
	SaveNetworkReport(context.Context, Report) error
	ListNetworkReports(context.Context) ([]Report, error)
}

const filesCommand = "find /etc/netplan /etc/network /etc/systemd/network /run/systemd/network /etc/NetworkManager/system-connections -maxdepth 2 -type f 2>/dev/null; true"

func Collect(ctx context.Context, runner sshclient.CommandRunner, primary netip.Addr) (Report, error) {
	report := Report{PrimaryIP: primary.String(), CollectedAt: time.Now().UTC(), Status: "review", Warnings: []string{"Параметры нового IP нужно проверять отдельно; смена сети пока не выполняется."}}
	run := func(command string) (string, error) {
		result, err := runner.Run(ctx, sshclient.CommandRequest{Command: command, Timeout: 15 * time.Second})
		if err != nil || len(result.Stdout) > 512*1024 {
			return "", errors.New("network probe failed")
		}
		return result.Stdout, nil
	}
	raw, err := run("ip -j address show")
	if err != nil {
		return report, err
	}
	var links []struct {
		Name string `json:"ifname"`
		Info []struct {
			IP     string `json:"local"`
			Prefix int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if json.Unmarshal([]byte(raw), &links) != nil {
		return report, errors.New("invalid address response")
	}
	matches := 0
	for _, link := range links {
		for _, info := range link.Info {
			ip, e := netip.ParseAddr(info.IP)
			if e != nil {
				continue
			}
			main := ip.Unmap() == primary.Unmap()
			if main {
				matches++
			}
			report.Addresses = append(report.Addresses, Address{link.Name, ip.String(), info.Prefix, main})
		}
	}
	for _, family := range []string{"-4", "-6"} {
		raw, e := run("ip -j " + family + " route show table all")
		var routes []Route
		if e != nil || json.Unmarshal([]byte(raw), &routes) != nil {
			report.Warnings = append(report.Warnings, "Не удалось прочитать маршруты "+family)
		} else {
			report.Routes = append(report.Routes, routes...)
		}
		raw, e = run("ip -j " + family + " rule show")
		var rules []Rule
		if e != nil || json.Unmarshal([]byte(raw), &rules) != nil {
			report.Warnings = append(report.Warnings, "Не удалось прочитать правила "+family)
		} else {
			report.Rules = append(report.Rules, rules...)
		}
	}
	for _, service := range []string{"systemd-networkd", "NetworkManager", "networking"} {
		raw, e := run("systemctl is-active " + service)
		if e == nil && strings.TrimSpace(raw) == "active" {
			report.Services = append(report.Services, service)
		}
	}
	raw, err = run(filesCommand)
	if err != nil {
		report.Warnings = append(report.Warnings, "Список конфигураций недоступен")
	}
	filenames := strings.Split(strings.TrimSpace(raw), "\n")
	sort.Strings(filenames)
	matchingFiles := 0
	for _, filename := range filenames {
		filename = strings.TrimSpace(filename)
		kind := configKind(filename)
		if kind == "" {
			continue
		}
		if len(report.Files) >= 64 {
			report.Warnings = append(report.Warnings, "Список конфигураций ограничен 64 файлами")
			break
		}
		file := ConfigFile{Path: filename, Kind: kind}
		// NetworkManager profiles may contain passwords. Only record their paths.
		if kind != "NetworkManager" {
			content, e := run("cat -- '" + strings.ReplaceAll(filename, "'", "'\\''") + "'")
			if e != nil {
				report.Warnings = append(report.Warnings, "Не прочитан файл "+filename)
			} else {
				file.MatchesPrimary = configContainsAddress(kind, content, primary)
			}
		}
		if file.MatchesPrimary {
			matchingFiles++
		}
		report.Files = append(report.Files, file)
	}
	if matches == 1 && matchingFiles == 1 && len(report.Warnings) == 1 {
		report.Status = "observed"
	} else {
		report.Warnings = append(report.Warnings, "Основной адрес не найден однозначно в интерфейсах и конфигурации.")
	}
	report.Warnings = append(report.Warnings, "Найденные файлы — свидетельства конфигурации, а не гарантия управления интерфейсом. Хостер автоматически не определён.")
	return report, nil
}

func configKind(filename string) string {
	if path.Clean(filename) != filename || strings.ContainsAny(filename, "\r\n\x00") {
		return ""
	}
	switch {
	case strings.HasPrefix(filename, "/etc/netplan/") && (strings.HasSuffix(filename, ".yaml") || strings.HasSuffix(filename, ".yml")):
		return "Netplan"
	case filename == "/etc/network/interfaces" || strings.HasPrefix(filename, "/etc/network/interfaces.d/"):
		return "interfaces"
	case (strings.HasPrefix(filename, "/etc/systemd/network/") || strings.HasPrefix(filename, "/run/systemd/network/")) && strings.HasSuffix(filename, ".network"):
		return "networkd"
	case strings.HasPrefix(filename, "/etc/NetworkManager/system-connections/"):
		return "NetworkManager"
	}
	return ""
}

func configContainsAddress(kind, content string, address netip.Addr) bool {
	equal := func(value string) bool {
		value = strings.TrimSpace(value)
		if prefix, err := netip.ParsePrefix(value); err == nil {
			return prefix.Addr().Unmap() == address.Unmap()
		}
		ip, err := netip.ParseAddr(value)
		return err == nil && ip.Unmap() == address.Unmap()
	}
	if kind == "Netplan" {
		var root yaml.Node
		if yaml.Unmarshal([]byte(content), &root) != nil {
			return false
		}
		var visit func(*yaml.Node) bool
		visit = func(node *yaml.Node) bool {
			if node.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(node.Content); i += 2 {
					if node.Content[i].Value == "nameservers" {
						continue
					}
					if node.Content[i].Value == "addresses" && node.Content[i+1].Kind == yaml.SequenceNode {
						for _, value := range node.Content[i+1].Content {
							if value.Kind == yaml.ScalarNode && equal(value.Value) {
								return true
							}
						}
					}
					if visit(node.Content[i+1]) {
						return true
					}
				}
				return false
			}
			for _, child := range node.Content {
				if visit(child) {
					return true
				}
			}
			return false
		}
		return visit(&root)
	}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if kind == "interfaces" {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "address" && equal(fields[1]) {
				return true
			}
		}
		if kind == "networkd" {
			key, value, ok := strings.Cut(line, "=")
			if ok && strings.TrimSpace(key) == "Address" && equal(value) {
				return true
			}
		}
	}
	return false
}
