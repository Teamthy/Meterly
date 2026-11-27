package pgstore

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/risk"
	"github.com/teamthy/meterly/internal/store"
)

type scanner interface {
	Scan(dest ...any) error
}

type riskRepo struct{ *repos }

func optUUIDString(s string) any {
	id, err := uuid.Parse(s)
	if err != nil {
		return nil
	}
	return id
}

func optUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func scanUUID(p *uuid.UUID) uuid.UUID {
	if p == nil {
		return uuid.Nil
	}
	return *p
}

func scanUser(p *uuid.UUID) string {
	if p == nil {
		return ""
	}
	return p.String()
}

func scanRiskRule(row scanner) (risk.Rule, error) {
	var rule risk.Rule
	var params []byte
	var updatedBy *uuid.UUID
	err := row.Scan(&rule.Code, &rule.Enabled, &rule.Action, &rule.WindowSeconds, &rule.Threshold, &params, &rule.UpdatedAt, &updatedBy)
	if err != nil {
		return risk.Rule{}, mapErr(err)
	}
	rule.Params = risk.ParseParams(params)
	rule.UpdatedAt = rule.UpdatedAt.UTC()
	if updatedBy != nil {
		rule.UpdatedBy = updatedBy.String()
	}
	return rule, nil
}

func (r riskRepo) ListRules(ctx context.Context) ([]risk.Rule, error) {
	rows, err := r.db.Query(ctx, `SELECT code, enabled, action, window_seconds, threshold, params, updated_at, updated_by FROM risk_rules ORDER BY code`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []risk.Rule
	for rows.Next() {
		rule, err := scanRiskRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, rows.Err()
}

func (r riskRepo) GetRule(ctx context.Context, code string) (risk.Rule, error) {
	rule, err := scanRiskRule(r.db.QueryRow(ctx, `SELECT code, enabled, action, window_seconds, threshold, params, updated_at, updated_by FROM risk_rules WHERE code = $1`, code))
	if err != nil {
		if err == store.ErrNotFound {
			return risk.Rule{}, risk.ErrNotFound
		}
		return risk.Rule{}, err
	}
	return rule, nil
}

func (r riskRepo) UpdateRule(ctx context.Context, rule risk.Rule) error {
	tag, err := r.db.Exec(ctx, `UPDATE risk_rules SET enabled=$2, action=$3, window_seconds=$4, threshold=$5, params=$6::jsonb, updated_at=now(), updated_by=$7
		WHERE code=$1`, rule.Code, rule.Enabled, string(rule.Action), rule.WindowSeconds, rule.Threshold, rule.Params.Marshal(), optUUIDString(rule.UpdatedBy))
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return risk.ErrNotFound
	}
	return nil
}

func (r riskRepo) InsertObservation(ctx context.Context, o risk.Observation) error {
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	if o.ObservedAt.IsZero() {
		o.ObservedAt = time.Now().UTC()
	}
	_, err := r.db.Exec(ctx, `INSERT INTO risk_observations (
		id, observed_at, kind, surface, user_id, meter_id, link_id, transaction_id,
		email_hmac, ip_hmac, device_hmac, phone_hmac, card_signature, amount_minor)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		o.ID, o.ObservedAt.UTC(), string(o.Kind), string(o.Surface),
		optUUIDString(o.UserID), optUUID(o.MeterID), optUUID(o.LinkID), optUUID(o.TransactionID),
		o.EmailHMAC, o.IPHMAC, o.DeviceHMAC, o.PhoneHMAC, o.CardSignature, o.AmountMinor)
	return mapErr(err)
}

func obsWhere(f risk.ObservationFilter) (string, []any) {
	q := ` WHERE 1=1`
	var args []any
	add := func(clause string, v any) {
		args = append(args, v)
		q += clause
	}
	n := 0
	ph := func() string {
		n++
		return "$" + itoa(n)
	}
	if f.Kind != "" {
		add(` AND kind = `+ph(), string(f.Kind))
	}
	if f.UserID != "" {
		add(` AND user_id = `+ph(), optUUIDString(f.UserID))
	}
	if f.MeterID != uuid.Nil {
		add(` AND meter_id = `+ph(), f.MeterID)
	}
	if f.LinkID != uuid.Nil {
		add(` AND link_id = `+ph(), f.LinkID)
	}
	if f.EmailHMAC != "" {
		add(` AND email_hmac = `+ph(), f.EmailHMAC)
	}
	if f.IPHMAC != "" {
		add(` AND ip_hmac = `+ph(), f.IPHMAC)
	}
	if f.DeviceHMAC != "" {
		add(` AND device_hmac = `+ph(), f.DeviceHMAC)
	}
	if f.CardSignature != "" {
		add(` AND card_signature = `+ph(), f.CardSignature)
	}
	if !f.Since.IsZero() {
		add(` AND observed_at >= `+ph(), f.Since.UTC())
	}
	return q, args
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func (r riskRepo) CountObservations(ctx context.Context, f risk.ObservationFilter) (int, error) {
	where, args := obsWhere(f)
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(*) FROM risk_observations`+where, args...).Scan(&n)
	return n, mapErr(err)
}

func (r riskRepo) DistinctCards(ctx context.Context, f risk.ObservationFilter) (int, error) {
	cf := f
	cf.Kind = ""
	where, args := obsWhere(cf)
	var n int
	err := r.db.QueryRow(ctx, `SELECT COUNT(DISTINCT card_signature) FROM risk_observations`+where+` AND card_signature <> ''`, args...).Scan(&n)
	return n, mapErr(err)
}

func (r riskRepo) RecentAmounts(ctx context.Context, userID string, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := r.db.Query(ctx, `SELECT amount_minor FROM risk_observations
		WHERE kind = 'payment_ok' AND user_id = $1 AND amount_minor > 0
		ORDER BY observed_at DESC LIMIT $2`, optUUIDString(userID), limit)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (r riskRepo) InsertDecision(ctx context.Context, d risk.Decision) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.Exec(ctx, `INSERT INTO risk_decisions (
		id, created_at, outcome, rule_code, surface, user_id, meter_id, link_id, transaction_id,
		amount_minor, email_hmac, ip_hmac, device_hmac, card_signature)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		d.ID, d.CreatedAt.UTC(), string(d.Outcome), d.RuleCode, string(d.Surface),
		optUUIDString(d.UserID), optUUID(d.MeterID), optUUID(d.LinkID), optUUID(d.TransactionID),
		d.AmountMinor, d.EmailHMAC, d.IPHMAC, d.DeviceHMAC, d.CardSignature)
	return mapErr(err)
}

func (r riskRepo) BindDecisionTxn(ctx context.Context, id, txnID uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `UPDATE risk_decisions SET transaction_id=$2 WHERE id=$1`, id, txnID)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return risk.ErrNotFound
	}
	return nil
}

func (r riskRepo) ListDecisionsByTxn(ctx context.Context, txnID uuid.UUID) ([]risk.Decision, error) {
	rows, err := r.db.Query(ctx, `SELECT id, created_at, outcome, rule_code, surface, user_id, meter_id, link_id, transaction_id,
		amount_minor, email_hmac, ip_hmac, device_hmac, card_signature
		FROM risk_decisions WHERE transaction_id=$1 ORDER BY created_at DESC`, txnID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []risk.Decision
	for rows.Next() {
		d, err := scanRiskDecision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func scanRiskDecision(row scanner) (risk.Decision, error) {
	var d risk.Decision
	var userID, meterID, linkID, txnID *uuid.UUID
	err := row.Scan(&d.ID, &d.CreatedAt, &d.Outcome, &d.RuleCode, &d.Surface, &userID, &meterID, &linkID, &txnID,
		&d.AmountMinor, &d.EmailHMAC, &d.IPHMAC, &d.DeviceHMAC, &d.CardSignature)
	if err != nil {
		return risk.Decision{}, mapErr(err)
	}
	d.CreatedAt = d.CreatedAt.UTC()
	d.UserID = scanUser(userID)
	d.MeterID = scanUUID(meterID)
	d.LinkID = scanUUID(linkID)
	d.TransactionID = scanUUID(txnID)
	return d, nil
}

func (r riskRepo) ListDenylist(ctx context.Context) ([]risk.DenyEntry, error) {
	rows, err := r.db.Query(ctx, `SELECT id, kind, value, reason, created_at, created_by FROM risk_denylist ORDER BY created_at DESC`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []risk.DenyEntry
	for rows.Next() {
		e, err := scanDeny(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanDeny(row scanner) (risk.DenyEntry, error) {
	var e risk.DenyEntry
	var createdBy *uuid.UUID
	if err := row.Scan(&e.ID, &e.Kind, &e.Value, &e.Reason, &e.CreatedAt, &createdBy); err != nil {
		return risk.DenyEntry{}, mapErr(err)
	}
	e.CreatedAt = e.CreatedAt.UTC()
	e.CreatedBy = scanUser(createdBy)
	return e, nil
}

func (r riskRepo) AddDenylist(ctx context.Context, e risk.DenyEntry) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.Exec(ctx, `INSERT INTO risk_denylist (id, kind, value, reason, created_at, created_by)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (kind, value) DO NOTHING`,
		e.ID, string(e.Kind), e.Value, e.Reason, e.CreatedAt.UTC(), optUUIDString(e.CreatedBy))
	return mapErr(err)
}

func (r riskRepo) RemoveDenylist(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `DELETE FROM risk_denylist WHERE id=$1`, id)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return risk.ErrNotFound
	}
	return nil
}

func (r riskRepo) MatchDenylist(ctx context.Context, kind risk.DenyKind, value string) (risk.DenyEntry, error) {
	e, err := scanDeny(r.db.QueryRow(ctx, `SELECT id, kind, value, reason, created_at, created_by FROM risk_denylist WHERE kind=$1 AND value=$2`, string(kind), value))
	if err != nil {
		if err == store.ErrNotFound {
			return risk.DenyEntry{}, risk.ErrNotFound
		}
		return risk.DenyEntry{}, err
	}
	return e, nil
}

func (r riskRepo) ListIPRanges(ctx context.Context) ([]risk.DenyEntry, error) {
	rows, err := r.db.Query(ctx, `SELECT id, kind, value, reason, created_at, created_by FROM risk_denylist WHERE kind='IP_RANGE'`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []risk.DenyEntry
	for rows.Next() {
		e, err := scanDeny(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r riskRepo) PauseLink(ctx context.Context, p risk.LinkPause) error {
	if p.PausedAt.IsZero() {
		p.PausedAt = time.Now().UTC()
	}
	_, err := r.db.Exec(ctx, `INSERT INTO risk_link_pauses (link_id, rule_code, reason, paused_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (link_id) DO UPDATE SET rule_code=EXCLUDED.rule_code, reason=EXCLUDED.reason, paused_at=EXCLUDED.paused_at`,
		p.LinkID, p.RuleCode, p.Reason, p.PausedAt.UTC())
	return mapErr(err)
}

func (r riskRepo) LinkPause(ctx context.Context, linkID uuid.UUID) (risk.LinkPause, error) {
	var p risk.LinkPause
	err := r.db.QueryRow(ctx, `SELECT link_id, rule_code, reason, paused_at FROM risk_link_pauses WHERE link_id=$1`, linkID).
		Scan(&p.LinkID, &p.RuleCode, &p.Reason, &p.PausedAt)
	if err != nil {
		if mapErr(err) == store.ErrNotFound {
			return risk.LinkPause{}, risk.ErrNotFound
		}
		return risk.LinkPause{}, mapErr(err)
	}
	p.PausedAt = p.PausedAt.UTC()
	return p, nil
}

func (r riskRepo) ClearLinkPause(ctx context.Context, linkID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `DELETE FROM risk_link_pauses WHERE link_id=$1`, linkID)
	return mapErr(err)
}
