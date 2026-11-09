package abuse

import (
	"context"
	"errors"
	"net/netip"
	"time"
)

var ErrNotFound = errors.New("abuse record not found")

type Checkpoint struct {
	PanelID, NodeUUID string
	Inode, Offset     int64
	PartialLine       string
}

type Incident struct {
	ID, PanelID, UserID, AnomalyType string
	Risk                             Risk
	Score                            int
	Reasons                          []string
	FirstSeen, LastSeen              time.Time
}

type Store interface {
	LoadCheckpoint(context.Context, string, string) (Checkpoint, error)
	SaveEvents(context.Context, Checkpoint, []Event) (int, error)
	ListIdentities(context.Context, time.Time) ([][2]string, error)
	LoadEvents(context.Context, string, string, time.Time) ([]Event, error)
	SaveAssessment(context.Context, Assessment) error
	OpenIncident(context.Context, Assessment, string, time.Duration) (Incident, bool, error)
	GetIncident(context.Context, string) (Incident, error)
	IgnoreIncident(context.Context, string, time.Time) error
	Cleanup(context.Context, time.Time, time.Time) error
	LoadIPIntelligence(context.Context, netip.Addr, time.Time) (Intelligence, bool, error)
	SaveIPIntelligence(context.Context, netip.Addr, Intelligence, time.Time) error
}
