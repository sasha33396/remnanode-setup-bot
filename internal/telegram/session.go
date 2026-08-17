package telegram

import (
	"net/netip"
	"time"
)

type wizardState uint8

type serverIPProvider uint8

const (
	serverIPProviderCherry serverIPProvider = iota + 1
	serverIPProviderRoyal
)

const (
	stateSelectingPanel wizardState = iota + 1
	stateSelectingIPPanel
	stateSelectingHost
	stateAwaitingName
	stateAwaitingIP
	stateAwaitingPassword
	statePreflighting
	stateAwaitingConfirmation
	stateDeploying
	stateAwaitingIPChangeQuery
	stateAwaitingIPChangeConfirmation
	stateAwaitingNewIP
	stateSelectingIPMode
	stateSelectingServerIPScope
	stateSelectingServerIPPanel
	stateAwaitingServerIPNodeQuery
	stateAwaitingServerCurrentIP
	stateAwaitingServerNewIP
	stateAwaitingServerIPPassword
)

type wizard struct {
	userID           int64
	chatID           int64
	nonce            string
	state            wizardState
	expiresAt        time.Time
	hosts            []Host
	panels           []Panel
	panel            Panel
	selected         Host
	nodeName         string
	vpsIP            netip.Addr
	password         []byte
	preflight        PreflightResult
	statusMsgID      int
	ipTarget         NodeIPChangeTarget
	serverIPProvider serverIPProvider
	serverCurrentIP  netip.Addr
	serverNewIP      netip.Addr
	serverUpdateNode bool
	expiryTimer      *time.Timer
}

func (w *wizard) clone() *wizard {
	if w == nil {
		return nil
	}
	result := *w
	result.hosts = append([]Host(nil), w.hosts...)
	result.panels = append([]Panel(nil), w.panels...)
	// Ordinary state snapshots intentionally never duplicate the password.
	result.password = nil
	result.expiryTimer = nil
	result.preflight.Warnings = append([]OperatorNotice(nil), w.preflight.Warnings...)
	result.ipTarget.DNSZones = append([]string(nil), w.ipTarget.DNSZones...)
	return &result
}

func (w *wizard) clear() {
	if w.expiryTimer != nil {
		w.expiryTimer.Stop()
		w.expiryTimer = nil
	}
	clearBytes(w.password)
	w.password = nil
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
