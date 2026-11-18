package leastload

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"remnanode-setup-bot/internal/networkinventory"
	sshclient "remnanode-setup-bot/internal/ssh"
)

type Session interface {
	sshclient.CommandRunner
	Close() error
}
type Connect func(context.Context, netip.Addr) (Session, error)

type UncertainStart struct{}

func (UncertainStart) Error() string {
	return "Не удалось подтвердить запуск; проверьте состояние операции"
}
func (e UncertainStart) OperatorMessage() string { return e.Error() }

func directory(id string) (string, error) {
	if len(id) != 36 || strings.Trim(id, "0123456789abcdef-") != "" {
		return "", Error("Некорректный идентификатор операции")
	}
	return "/var/lib/remnanode-ip/" + id, nil
}

// Start stages private backups and an independent rollback timer BEFORE any
// network file can be changed. The worker survives SSH and bot disconnection.
func Start(ctx context.Context, r Session, id string, p Plan) error {
	dir, err := directory(id)
	if err != nil {
		return err
	}
	content, err := run(ctx, r, "cat -- "+quote(p.File), nil)
	if err != nil || digest([]byte(content)) != p.FileHash {
		return Error("Netplan изменился после проверки; подготовьте новый план")
	}
	updated, mode, _, err := rewrite([]byte(content), p.Section, netip.MustParseAddr(p.OldIP), netip.MustParseAddr(p.NewIP))
	if err != nil {
		return err
	}
	if (p.Mode == "replace" && mode != "replace") || (p.Mode == "activate" && mode != "existing") {
		return Error("Режим изменения сети устарел")
	}
	if _, err = run(ctx, r, "umask 077; mkdir -p /var/lib/remnanode-ip && mkdir -- "+quote(dir)+" && cp -p -- "+quote(p.File)+" "+quote(dir+"/original")+" && chmod 600 "+quote(dir+"/original"), nil); err != nil {
		return Error("Не удалось подготовить уникальную резервную копию")
	}
	if _, err = run(ctx, r, "umask 077; cat > "+quote(dir+"/updated"), updated); err != nil {
		return Error("Не удалось подготовить новый Netplan")
	}
	rollback := "#!/bin/sh\nset -u\ncd " + quote(dir) + " || exit 1\nprintf rollback > rollback-choice\nif ln rollback-choice decision 2>/dev/null; then\n if [ -f changed ]; then\n  cp -p original " + quote(p.File) + " && timeout 40 netplan apply && echo rolled_back > status || echo rollback_failed > status\n else echo aborted > status; fi\nfi\n"
	// The global flock serializes managed transactions on the physical server,
	// including Nodes belonging to different panels.
	worker := "#!/bin/sh\nset -u\ncd " + quote(dir) + " || exit 1\nexec 9>/run/remnanode-ip-change.lock\nflock -n 9 || { echo busy > status; exit 1; }\n" +
		"[ \"$(sha256sum " + quote(p.File) + " | cut -d ' ' -f1)\" = " + quote(p.FileHash) + " ] || { echo stale > status; exit 1; }\n" +
		"[ ! -e decision ] || exit 1\necho applying > status\ntouch changed\ncp updated " + quote(p.File) + " || { sh rollback.sh; exit 1; }\n" +
		"timeout 20 netplan generate && timeout 40 netplan apply || { sh rollback.sh; exit 1; }\necho waiting > status\n" +
		"n=0; while [ ! -e decision ] && [ $n -lt 100 ]; do sleep 1; n=$((n+1)); done\n" +
		"if [ ! -e decision ]; then sh rollback.sh; fi\nif [ \"$(cat decision 2>/dev/null)\" = confirm ]; then echo committed > status; fi\n"
	for filename, script := range map[string]string{"rollback.sh": rollback, "worker.sh": worker} {
		if _, err = run(ctx, r, "umask 077; cat > "+quote(dir+"/"+filename), []byte(script)); err != nil {
			return Error("Не удалось подготовить автономный откат")
		}
	}
	unit := "remnanode-ip-" + id
	if _, err = run(ctx, r, "systemd-run --quiet --unit="+unit+"-rollback --on-active=180s /bin/sh "+quote(dir+"/rollback.sh"), nil); err != nil {
		return Error("Не удалось запланировать независимый откат; сеть не менялась")
	}
	if _, err = run(ctx, r, "systemd-run --quiet --unit="+unit+" --property=RuntimeMaxSec=175 /bin/sh "+quote(dir+"/worker.sh"), nil); err != nil {
		return UncertainStart{}
	}
	return nil
}

func remoteStatus(ctx context.Context, r Session, id string) (string, error) {
	dir, err := directory(id)
	if err != nil {
		return "", err
	}
	result, err := run(ctx, r, "cat -- "+quote(dir+"/status"), nil)
	return strings.TrimSpace(result), err
}

// Verify checks both desired address and preservation of the other addresses
// and static/default routes. It never edits network configuration.
func Verify(ctx context.Context, r Session, p Plan) error {
	content, err := run(ctx, r, "cat -- "+quote(p.File), nil)
	if err != nil || p.TargetHash == "" || digest([]byte(content)) != p.TargetHash {
		return Error("Сохранённый Netplan не совпадает с проверенным планом")
	}
	raw, err := run(ctx, r, "ip -j address show", nil)
	if err != nil {
		return Error("Не удалось проверить адреса")
	}
	var links []struct {
		Name      string `json:"ifname"`
		Addresses []struct {
			IP     string `json:"local"`
			Prefix int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if json.Unmarshal([]byte(raw), &links) != nil {
		return Error("Некорректный ответ интерфейсов")
	}
	found := map[string]bool{}
	newFound := false
	for _, l := range links {
		for _, a := range l.Addresses {
			found[fmt.Sprintf("%s|%s/%d", l.Name, a.IP, a.Prefix)] = true
			if a.IP == p.NewIP {
				if l.Name != p.Interface || a.Prefix != p.Prefix {
					return Error("Новый адрес появился с неверной привязкой")
				}
				newFound = true
			}
		}
	}
	if !newFound {
		return Error("Новый IP ещё не назначен интерфейсу")
	}
	for _, a := range p.Before.Addresses {
		if a.IP == p.OldIP && p.Mode == "replace" {
			continue
		}
		ip, e := netip.ParseAddr(a.IP)
		if e == nil && ip.IsLinkLocalUnicast() {
			continue
		}
		if !found[fmt.Sprintf("%s|%s/%d", a.Interface, a.IP, a.Prefix)] {
			return Error("Не подтверждена сохранность дополнительного адреса")
		}
	}
	for _, family := range []string{"-4", "-6"} {
		ruleData, e := run(ctx, r, "ip -j "+family+" rule show", nil)
		if e != nil {
			return Error("Не удалось проверить правила маршрутизации")
		}
		var rules []networkinventory.Rule
		if json.Unmarshal([]byte(ruleData), &rules) != nil {
			return Error("Некорректный ответ правил маршрутизации")
		}
		beforeRules := []networkinventory.Rule{}
		for _, rule := range p.Before.Rules {
			if rule.Family == family {
				rule.Family = ""
				beforeRules = append(beforeRules, rule)
			}
		}
		left, _ := json.Marshal(beforeRules)
		right, _ := json.Marshal(rules)
		if string(left) != string(right) && !(len(beforeRules) == 0 && len(rules) == 0) {
			return Error("Правила маршрутизации изменились")
		}
		raw, err := run(ctx, r, "ip -j "+family+" route show table all", nil)
		if err != nil {
			return Error("Не удалось проверить маршруты")
		}
		var routes []networkinventory.Route
		if json.Unmarshal([]byte(raw), &routes) != nil {
			return Error("Некорректный ответ маршрутов")
		}
		for _, before := range p.Before.Routes {
			if before.Family != family || (before.Protocol != "static" && before.Destination != "default") {
				continue
			}
			if p.Mode == "replace" && before.Source == p.OldIP {
				before.Source = p.NewIP
			}
			matched := false
			for _, after := range routes {
				onlink := false
				for _, f := range after.Flags {
					onlink = onlink || f == "onlink"
				}
				if before.Destination == after.Destination && before.Gateway == after.Gateway && before.Interface == after.Interface && before.Source == after.Source && fmt.Sprint(before.Table) == fmt.Sprint(after.Table) && before.OnLink == onlink && sameInt(before.Metric, after.Metric) {
					matched = true
				}
			}
			if !matched {
				return Error("Не подтверждена сохранность маршрутов")
			}
		}
	}
	return nil
}
func sameInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Finish is also the recovery entry point after a bot restart. A pending
// transaction is never launched twice. Only a confirmed remote commit permits
// panel/DNS mutation; timeout/rollback cannot be mistaken for success.
func Finish(ctx context.Context, connect Connect, id string, p Plan) (string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Second)
	defer cancel()
	dir, err := directory(id)
	if err != nil {
		return "", err
	}
	for {
		for _, rawIP := range []string{p.NewIP, p.OldIP} {
			attempt, cancelAttempt := context.WithTimeout(waitCtx, 15*time.Second)
			conn, e := connect(attempt, netip.MustParseAddr(rawIP))
			if e == nil {
				status, statusErr := remoteStatus(attempt, conn, id)
				if statusErr == nil {
					switch status {
					case "rolled_back", "rollback_failed", "aborted", "busy", "stale":
						conn.Close()
						cancelAttempt()
						return status, Error("Сеть не подтверждена: " + status)
					case "committed":
						if rawIP == p.NewIP && Verify(attempt, conn, p) == nil {
							conn.Close()
							cancelAttempt()
							return "committed", nil
						}
					case "waiting":
						if rawIP == p.NewIP && Verify(attempt, conn, p) == nil {
							_, _ = run(attempt, conn, "cd "+quote(dir)+" && printf confirm > confirm-choice && ln confirm-choice decision", nil)
						}
					}
				}
				conn.Close()
			}
			cancelAttempt()
		}
		select {
		case <-waitCtx.Done():
			return "unknown", Error("Состояние сети не подтверждено. Панель и DNS не менялись; повторите проверку операции")
		case <-time.After(2 * time.Second):
		}
	}
}
