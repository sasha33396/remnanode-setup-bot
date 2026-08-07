package telegram

import (
	"net/netip"
	"time"
)

type wizardState uint8

const (
	stateSelectingHost wizardState = iota + 1
	stateAwaitingName
	stateAwaitingIP
	stateAwaitingPassword
	statePreflighting
	stateAwaitingConfirmation
	stateDeploying
)

type wizard struct {
	userID      int64
	chatID      int64
	nonce       string
	state       wizardState
	expiresAt   time.Time
	hosts       []Host
	selected    Host
	nodeName    string
	vpsIP       netip.Addr
	password    []byte
	preflight   PreflightResult
	statusMsgID int
	expiryTimer *time.Timer
}

func (w *wizard) clone() *wizard {
	if w == nil {
		return nil
	}
	result := *w
	result.hosts = append([]Host(nil), w.hosts...)
	// Ordinary state snapshots intentionally never duplicate the password.
	result.password = nil
	result.expiryTimer = nil
	result.preflight.SafeWarnings = append([]string(nil), w.preflight.SafeWarnings...)
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
