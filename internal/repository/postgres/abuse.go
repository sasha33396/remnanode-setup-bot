package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"remnanode-setup-bot/internal/abuse"
)

func (r *Repository) LoadCheckpoint(ctx context.Context, panelID, nodeUUID string) (abuse.Checkpoint, error) {
	checkpoint := abuse.Checkpoint{PanelID: panelID, NodeUUID: nodeUUID}
	err := r.pool.QueryRow(ctx, `SELECT inode, byte_offset, partial_line FROM abuse_log_checkpoints WHERE panel_id=$1 AND node_uuid=$2`, panelID, nodeUUID).Scan(&checkpoint.Inode, &checkpoint.Offset, &checkpoint.PartialLine)
	if errors.Is(err, pgx.ErrNoRows) {
		return checkpoint, nil
	}
	if err != nil {
		return abuse.Checkpoint{}, errors.New("load abuse log checkpoint failed")
	}
	return checkpoint, nil
}

func (r *Repository) SaveEvents(ctx context.Context, checkpoint abuse.Checkpoint, events []abuse.Event) (int, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, errors.New("begin abuse event transaction failed")
	}
	defer tx.Rollback(ctx)
	inserted := 0
	for _, event := range events {
		fingerprint := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%d\x00%s", event.PanelID, event.NodeUUID, event.OccurredAt.Format(time.RFC3339Nano), event.ClientIP, event.ClientPort, event.Destination, event.DestinationPort, event.UserID)))
		var destinationIP any
		if event.DestinationIP.IsValid() {
			destinationIP = event.DestinationIP.String()
		}
		result, execErr := tx.Exec(ctx, `INSERT INTO abuse_events (fingerprint,panel_id,node_uuid,node_name,node_ip,occurred_at,client_ip,client_port,destination_ip,destination,destination_port,network,inbound,outbound,user_id,accepted,blocked,asn,provider,network_category)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20) ON CONFLICT (fingerprint) DO NOTHING`, fingerprint[:], event.PanelID, event.NodeUUID, event.NodeName, event.NodeIP.String(), event.OccurredAt, event.ClientIP.String(), event.ClientPort, destinationIP, event.Destination, event.DestinationPort, event.Network, event.Inbound, event.Outbound, event.UserID, event.Accepted, event.Blocked, event.ASN, unknown(event.Provider), unknown(event.Category))
		if execErr != nil {
			return 0, errors.New("insert abuse event failed")
		}
		inserted += int(result.RowsAffected())
	}
	_, err = tx.Exec(ctx, `INSERT INTO abuse_log_checkpoints (panel_id,node_uuid,inode,byte_offset,partial_line,updated_at) VALUES ($1,$2,$3,$4,$5,now()) ON CONFLICT (panel_id,node_uuid) DO UPDATE SET inode=EXCLUDED.inode,byte_offset=EXCLUDED.byte_offset,partial_line=EXCLUDED.partial_line,updated_at=now()`, checkpoint.PanelID, checkpoint.NodeUUID, checkpoint.Inode, checkpoint.Offset, checkpoint.PartialLine)
	if err != nil {
		return 0, errors.New("save abuse log checkpoint failed")
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, errors.New("commit abuse event transaction failed")
	}
	return inserted, nil
}

func (r *Repository) ListIdentities(ctx context.Context, since time.Time) ([][2]string, error) {
	rows, err := r.pool.Query(ctx, `SELECT DISTINCT panel_id,user_id FROM abuse_events WHERE occurred_at >= $1`, since)
	if err != nil {
		return nil, errors.New("list abuse identities failed")
	}
	defer rows.Close()
	var result [][2]string
	for rows.Next() {
		var identity [2]string
		if err := rows.Scan(&identity[0], &identity[1]); err != nil {
			return nil, errors.New("scan abuse identity failed")
		}
		result = append(result, identity)
	}
	if rows.Err() != nil {
		return nil, errors.New("read abuse identities failed")
	}
	return result, nil
}

func (r *Repository) LoadEvents(ctx context.Context, panelID, userID string, since time.Time) ([]abuse.Event, error) {
	rows, err := r.pool.Query(ctx, `SELECT node_uuid::text,node_name,host(node_ip),occurred_at,host(client_ip),client_port,COALESCE(host(destination_ip),''),destination,destination_port,network,inbound,outbound,accepted,blocked,asn,provider,network_category FROM abuse_events WHERE panel_id=$1 AND user_id=$2 AND occurred_at >= $3 ORDER BY occurred_at`, panelID, userID, since)
	if err != nil {
		return nil, errors.New("load abuse events failed")
	}
	defer rows.Close()
	var result []abuse.Event
	for rows.Next() {
		var event abuse.Event
		var nodeIP, clientIP, destinationIP string
		event.PanelID, event.UserID = panelID, userID
		if err := rows.Scan(&event.NodeUUID, &event.NodeName, &nodeIP, &event.OccurredAt, &clientIP, &event.ClientPort, &destinationIP, &event.Destination, &event.DestinationPort, &event.Network, &event.Inbound, &event.Outbound, &event.Accepted, &event.Blocked, &event.ASN, &event.Provider, &event.Category); err != nil {
			return nil, errors.New("scan abuse event failed")
		}
		event.NodeIP, _ = netip.ParseAddr(nodeIP)
		event.ClientIP, _ = netip.ParseAddr(clientIP)
		event.DestinationIP, _ = netip.ParseAddr(destinationIP)
		result = append(result, event)
	}
	if rows.Err() != nil {
		return nil, errors.New("read abuse events failed")
	}
	return result, nil
}

func (r *Repository) SaveAssessment(ctx context.Context, assessment abuse.Assessment) error {
	reasonsJSON, err := json.Marshal(assessment.Reasons)
	if err != nil {
		return errors.New("encode abuse assessment reasons failed")
	}
	_, err = r.pool.Exec(ctx, `INSERT INTO abuse_user_aggregates (panel_id,user_id,smtp25_attempts_10m,submission_attempts_1h,unique_mail_destinations_1h,unique_ips_1h,unique_ips_24h,unique_asns_24h,risk,score,reasons,first_seen,last_seen,updated_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,now())
		ON CONFLICT (panel_id,user_id) DO UPDATE SET smtp25_attempts_10m=EXCLUDED.smtp25_attempts_10m,submission_attempts_1h=EXCLUDED.submission_attempts_1h,unique_mail_destinations_1h=EXCLUDED.unique_mail_destinations_1h,unique_ips_1h=EXCLUDED.unique_ips_1h,unique_ips_24h=EXCLUDED.unique_ips_24h,unique_asns_24h=EXCLUDED.unique_asns_24h,risk=EXCLUDED.risk,score=EXCLUDED.score,reasons=EXCLUDED.reasons,first_seen=EXCLUDED.first_seen,last_seen=EXCLUDED.last_seen,updated_at=now()`, assessment.PanelID, assessment.UserID, assessment.SMTP25Attempts10M, assessment.SubmissionAttempts1H, assessment.UniqueMailDestinations1H, assessment.UniqueIPs1H, assessment.UniqueIPs24H, assessment.UniqueASNs24H, assessment.Risk, assessment.Score, json.RawMessage(reasonsJSON), nullableTime(assessment.FirstSeen), nullableTime(assessment.LastSeen))
	if err != nil {
		return errors.New("save abuse assessment failed")
	}
	return nil
}

func (r *Repository) OpenIncident(ctx context.Context, assessment abuse.Assessment, anomalyType string, cooldown time.Duration) (abuse.Incident, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return abuse.Incident{}, false, errors.New("begin abuse incident transaction failed")
	}
	defer tx.Rollback(ctx)
	now := time.Now().UTC()
	var incident abuse.Incident
	var previousRisk string
	var alertedAt, ignoredUntil *time.Time
	var storedReasons []byte
	err = tx.QueryRow(ctx, `SELECT id,risk,score,reasons,first_seen,last_seen,last_alerted_at,ignored_until FROM abuse_incidents WHERE panel_id=$1 AND user_id=$2 AND anomaly_type=$3 FOR UPDATE`, assessment.PanelID, assessment.UserID, anomalyType).Scan(&incident.ID, &previousRisk, &incident.Score, &storedReasons, &incident.FirstSeen, &incident.LastSeen, &alertedAt, &ignoredUntil)
	if err == nil && json.Unmarshal(storedReasons, &incident.Reasons) != nil {
		return abuse.Incident{}, false, errors.New("decode abuse incident reasons failed")
	}
	reasonsJSON, marshalErr := json.Marshal(assessment.Reasons)
	if marshalErr != nil {
		return abuse.Incident{}, false, errors.New("encode abuse incident reasons failed")
	}
	shouldAlert := false
	if errors.Is(err, pgx.ErrNoRows) {
		incident.ID, err = randomIncidentID()
		if err != nil {
			return abuse.Incident{}, false, err
		}
		shouldAlert = true
		_, err = tx.Exec(ctx, `INSERT INTO abuse_incidents (id,panel_id,user_id,anomaly_type,risk,score,reasons,first_seen,last_seen,last_alerted_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, incident.ID, assessment.PanelID, assessment.UserID, anomalyType, assessment.Risk, assessment.Score, json.RawMessage(reasonsJSON), assessment.FirstSeen, assessment.LastSeen, now)
	} else if err == nil {
		shouldAlert = (ignoredUntil == nil || ignoredUntil.Before(now)) && (alertedAt == nil || alertedAt.Add(cooldown).Before(now) || riskRank(assessment.Risk) > riskRank(abuse.Risk(previousRisk)))
		_, err = tx.Exec(ctx, `UPDATE abuse_incidents SET risk=$2,score=$3,reasons=$4,first_seen=LEAST(first_seen,$5),last_seen=GREATEST(last_seen,$6),last_alerted_at=CASE WHEN $7 THEN $8 ELSE last_alerted_at END,updated_at=now() WHERE id=$1`, incident.ID, assessment.Risk, assessment.Score, json.RawMessage(reasonsJSON), assessment.FirstSeen, assessment.LastSeen, shouldAlert, now)
	}
	if err != nil {
		return abuse.Incident{}, false, errors.New("upsert abuse incident failed")
	}
	if err := tx.Commit(ctx); err != nil {
		return abuse.Incident{}, false, errors.New("commit abuse incident failed")
	}
	incident.PanelID, incident.UserID, incident.AnomalyType, incident.Risk, incident.Score, incident.Reasons, incident.FirstSeen, incident.LastSeen = assessment.PanelID, assessment.UserID, anomalyType, assessment.Risk, assessment.Score, assessment.Reasons, assessment.FirstSeen, assessment.LastSeen
	return incident, shouldAlert, nil
}

func (r *Repository) GetIncident(ctx context.Context, id string) (abuse.Incident, error) {
	var item abuse.Incident
	var reasonsJSON []byte
	err := r.pool.QueryRow(ctx, `SELECT id,panel_id,user_id,anomaly_type,risk,score,reasons,first_seen,last_seen FROM abuse_incidents WHERE id=$1`, id).Scan(&item.ID, &item.PanelID, &item.UserID, &item.AnomalyType, &item.Risk, &item.Score, &reasonsJSON, &item.FirstSeen, &item.LastSeen)
	if errors.Is(err, pgx.ErrNoRows) {
		return abuse.Incident{}, abuse.ErrNotFound
	}
	if err != nil {
		return abuse.Incident{}, errors.New("load abuse incident failed")
	}
	if err := json.Unmarshal(reasonsJSON, &item.Reasons); err != nil {
		return abuse.Incident{}, errors.New("decode abuse incident reasons failed")
	}
	return item, nil
}

func (r *Repository) IgnoreIncident(ctx context.Context, id string, until time.Time) error {
	result, err := r.pool.Exec(ctx, `UPDATE abuse_incidents SET ignored_until=$2,updated_at=now() WHERE id=$1`, id, until)
	if err != nil {
		return errors.New("ignore abuse incident failed")
	}
	if result.RowsAffected() == 0 {
		return abuse.ErrNotFound
	}
	return nil
}

func (r *Repository) Cleanup(ctx context.Context, eventsBefore, cacheBefore time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return errors.New("begin abuse monitor cleanup failed")
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM abuse_events WHERE occurred_at < $1`, eventsBefore); err != nil {
		return errors.New("cleanup abuse events failed")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM abuse_user_aggregates WHERE last_seen < $1`, eventsBefore); err != nil {
		return errors.New("cleanup abuse aggregates failed")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM abuse_ip_cache WHERE expires_at < $1`, cacheBefore); err != nil {
		return errors.New("cleanup IP intelligence cache failed")
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.New("commit abuse monitor cleanup failed")
	}
	return nil
}

func (r *Repository) LoadIPIntelligence(ctx context.Context, ip netip.Addr, now time.Time) (abuse.Intelligence, bool, error) {
	var info abuse.Intelligence
	err := r.pool.QueryRow(ctx, `SELECT asn,provider,network_category FROM abuse_ip_cache WHERE ip=$1 AND expires_at>$2`, ip.String(), now).Scan(&info.ASN, &info.Provider, &info.Category)
	if errors.Is(err, pgx.ErrNoRows) {
		return abuse.Intelligence{}, false, nil
	}
	if err != nil {
		return abuse.Intelligence{}, false, errors.New("load IP intelligence cache failed")
	}
	return info, true, nil
}

func (r *Repository) SaveIPIntelligence(ctx context.Context, ip netip.Addr, info abuse.Intelligence, expiresAt time.Time) error {
	_, err := r.pool.Exec(ctx, `INSERT INTO abuse_ip_cache (ip,asn,provider,network_category,expires_at,updated_at) VALUES ($1,$2,$3,$4,$5,now()) ON CONFLICT (ip) DO UPDATE SET asn=EXCLUDED.asn,provider=EXCLUDED.provider,network_category=EXCLUDED.network_category,expires_at=EXCLUDED.expires_at,updated_at=now()`, ip.String(), info.ASN, unknown(info.Provider), unknown(info.Category), expiresAt)
	if err != nil {
		return errors.New("save IP intelligence cache failed")
	}
	return nil
}

func randomIncidentID() (string, error) {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", errors.New("generate abuse incident ID failed")
	}
	return hex.EncodeToString(value[:]), nil
}
func unknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "UNKNOWN"
	}
	return value
}
func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
func riskRank(risk abuse.Risk) int {
	switch risk {
	case abuse.RiskCritical:
		return 4
	case abuse.RiskHigh:
		return 3
	case abuse.RiskMedium:
		return 2
	default:
		return 1
	}
}

var _ abuse.Store = (*Repository)(nil)
