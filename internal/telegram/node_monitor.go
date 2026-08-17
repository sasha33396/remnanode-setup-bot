package telegram

import (
	"context"
	"errors"
	"time"
)

const (
	defaultNodeMonitorInterval      = 5 * time.Minute
	defaultNodeAlertRepeatInterval  = 15 * time.Minute
	defaultNodeMonitorConfirmations = 2
)

type nodeAlertState struct {
	consecutive      int
	lastNotification map[int64]time.Time
}

// NodeMonitor periodically checks connected nodes and notifies every allowed
// operator when a node remains below its panel-relative online threshold.
type NodeMonitor struct {
	app            Application
	messenger      Messenger
	users          []int64
	interval       time.Duration
	repeatInterval time.Duration
	confirmations  int
	policy         NodePolicy
	states         map[string]*nodeAlertState
	now            func() time.Time
}

func NewNodeMonitor(allowedUsers []int64, app Application, messenger Messenger, interval, repeatInterval time.Duration, confirmations int, policy NodePolicy) (*NodeMonitor, error) {
	if len(allowedUsers) == 0 || app == nil || messenger == nil {
		return nil, errors.New("invalid node monitor configuration")
	}
	users := make([]int64, 0, len(allowedUsers))
	seen := make(map[int64]struct{}, len(allowedUsers))
	for _, userID := range allowedUsers {
		if userID <= 0 {
			return nil, errors.New("invalid node monitor user ID")
		}
		if _, exists := seen[userID]; exists {
			continue
		}
		seen[userID] = struct{}{}
		users = append(users, userID)
	}
	if interval <= 0 {
		interval = defaultNodeMonitorInterval
	}
	if repeatInterval <= 0 {
		repeatInterval = defaultNodeAlertRepeatInterval
	}
	if confirmations <= 0 {
		confirmations = defaultNodeMonitorConfirmations
	}
	return &NodeMonitor{
		app:            app,
		messenger:      messenger,
		users:          users,
		interval:       interval,
		repeatInterval: repeatInterval,
		confirmations:  confirmations,
		policy:         normalizeNodePolicy(policy),
		states:         make(map[string]*nodeAlertState),
		now:            time.Now,
	}, nil
}

// Run keeps monitoring until the parent context is cancelled. Temporary API
// and Telegram failures are retried on the next interval and do not stop bot
// polling.
func (m *NodeMonitor) Run(ctx context.Context) error {
	_ = m.sample(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = m.sample(ctx)
		}
	}
}

func (m *NodeMonitor) sample(ctx context.Context) error {
	panels, err := m.app.ListPanels(ctx)
	if err != nil {
		return err
	}
	nodes, err := m.app.ListNodes(ctx)
	if err != nil {
		return err
	}
	snapshots := classifyNodePanels(panels, nodes, m.policy)
	now := m.now()
	seenNodes := make(map[string]struct{}, len(nodes))
	for _, snapshot := range snapshots {
		for _, node := range snapshot.All {
			seenNodes[nodeMonitorKey(node)] = struct{}{}
		}
		for _, node := range snapshot.Critical {
			key := nodeMonitorKey(node)
			state := m.states[key]
			if state == nil {
				state = &nodeAlertState{lastNotification: make(map[int64]time.Time)}
				m.states[key] = state
			}
			state.consecutive++
			if state.consecutive < m.confirmations {
				continue
			}
			keyboard := Keyboard{Inline: [][]Button{{{Text: "📡 Открыть карточку ноды", CallbackData: "nodes:o:" + node.UUID}}}}
			for _, userID := range m.users {
				lastNotification := state.lastNotification[userID]
				if !lastNotification.IsZero() && now.Before(lastNotification.Add(m.repeatInterval)) {
					continue
				}
				if _, sendErr := m.messenger.SendMessage(ctx, userID, formatNodeAlert(snapshot, node), keyboard); sendErr == nil {
					state.lastNotification[userID] = now
				}
			}
		}
		for _, node := range snapshot.Active {
			key := nodeMonitorKey(node)
			state := m.states[key]
			if state == nil {
				continue
			}
			for _, userID := range m.users {
				if state.lastNotification[userID].IsZero() {
					continue
				}
				_, _ = m.messenger.SendMessage(ctx, userID, formatNodeRecovery(snapshot, node), Keyboard{Inline: [][]Button{{{Text: "📡 Открыть карточку ноды", CallbackData: "nodes:o:" + node.UUID}}}})
			}
			delete(m.states, key)
		}
		for _, node := range snapshot.Disabled {
			delete(m.states, nodeMonitorKey(node))
		}
	}
	for key := range m.states {
		if _, exists := seenNodes[key]; !exists {
			// Removed nodes are no longer actionable. Disconnected and
			// missing-metric nodes intentionally retain their incident state so a
			// temporary telemetry gap cannot create a duplicate alert later.
			delete(m.states, key)
		}
	}
	return nil
}

func nodeMonitorKey(node NodeSummary) string {
	return node.PanelID + ":" + node.UUID
}
