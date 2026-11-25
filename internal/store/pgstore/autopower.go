package pgstore

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/teamthy/meterly/internal/autopower"
	"github.com/teamthy/meterly/internal/store"
)

type autoPowerRepo struct{ *repos }

// ---- saved_payment_methods -------------------------------------------------

const payMethodCols = `id, user_id::text, provider, source_transaction_id,
	auth_code_ciphertext, email_ciphertext, key_version,
	card_brand, last4, exp_month, exp_year, signature,
	consent_version, consent_text, consented_at, created_at, deleted_at`

func scanPaymentMethod(row pgx.Row) (*autopower.SavedPaymentMethod, error) {
	var m autopower.SavedPaymentMethod
	var deletedAt *time.Time
	if err := row.Scan(
		&m.ID, &m.UserID, &m.Provider, &m.SourceTransactionID,
		&m.AuthCodeCiphertext, &m.EmailCiphertext, &m.KeyVersion,
		&m.CardBrand, &m.Last4, &m.ExpMonth, &m.ExpYear, &m.Signature,
		&m.ConsentVersion, &m.ConsentText, &m.ConsentedAt, &m.CreatedAt, &deletedAt,
	); err != nil {
		return nil, mapErr(err)
	}
	m.ConsentedAt = m.ConsentedAt.UTC()
	m.CreatedAt = m.CreatedAt.UTC()
	if deletedAt != nil {
		t := deletedAt.UTC()
		m.DeletedAt = &t
	}
	return &m, nil
}

func (r autoPowerRepo) CreatePaymentMethod(ctx context.Context, m *autopower.SavedPaymentMethod) error {
	uid, err := uuid.Parse(m.UserID)
	if err != nil {
		return store.ErrNotFound
	}
	if m.ID == uuid.Nil {
		m.ID = uuid.New()
	}
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	} else {
		m.CreatedAt = m.CreatedAt.UTC()
	}
	m.ConsentedAt = m.ConsentedAt.UTC()
	_, err = r.db.Exec(ctx, `
		INSERT INTO saved_payment_methods (
			id, user_id, provider, source_transaction_id,
			auth_code_ciphertext, email_ciphertext, key_version,
			card_brand, last4, exp_month, exp_year, signature,
			consent_version, consent_text, consented_at, created_at, deleted_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
		m.ID, uid, m.Provider, m.SourceTransactionID,
		m.AuthCodeCiphertext, m.EmailCiphertext, m.KeyVersion,
		m.CardBrand, m.Last4, m.ExpMonth, m.ExpYear, m.Signature,
		m.ConsentVersion, m.ConsentText, m.ConsentedAt, m.CreatedAt, m.DeletedAt,
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	return mapErr(err)
}

func (r autoPowerRepo) GetPaymentMethod(ctx context.Context, id uuid.UUID) (*autopower.SavedPaymentMethod, error) {
	return scanPaymentMethod(r.db.QueryRow(ctx, `SELECT `+payMethodCols+` FROM saved_payment_methods WHERE id = $1`, id))
}

func (r autoPowerRepo) GetPaymentMethodForUpdate(ctx context.Context, id uuid.UUID) (*autopower.SavedPaymentMethod, error) {
	if !r.inTx {
		return nil, errors.New("payment method lock requires transaction")
	}
	return scanPaymentMethod(r.db.QueryRow(ctx, `SELECT `+payMethodCols+` FROM saved_payment_methods WHERE id = $1 FOR UPDATE`, id))
}

func (r autoPowerRepo) GetActivePaymentMethodBySourceTxn(ctx context.Context, userID string, txnID uuid.UUID) (*autopower.SavedPaymentMethod, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, store.ErrNotFound
	}
	return scanPaymentMethod(r.db.QueryRow(ctx, `
		SELECT `+payMethodCols+` FROM saved_payment_methods
		WHERE user_id = $1 AND source_transaction_id = $2 AND deleted_at IS NULL`, uid, txnID))
}

func (r autoPowerRepo) ListPaymentMethods(ctx context.Context, userID string) ([]*autopower.SavedPaymentMethod, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return []*autopower.SavedPaymentMethod{}, nil
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+payMethodCols+` FROM saved_payment_methods
		WHERE user_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC, id ASC`, uid)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*autopower.SavedPaymentMethod
	for rows.Next() {
		m, err := scanPaymentMethod(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r autoPowerRepo) DeletePaymentMethod(ctx context.Context, id uuid.UUID, at time.Time) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE saved_payment_methods
		SET deleted_at = $2
		WHERE id = $1 AND deleted_at IS NULL`, id, at.UTC())
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

// ---- autopower_rules -------------------------------------------------------

const ruleCols = `id, user_id::text, meter_id, payment_method_id, trigger_type,
	amount_minor, monthly_cap_minor, trim(currency), delivery_preference,
	frequency, days_of_week, days_of_month, time_of_day, timezone, ends_at,
	low_balance_milli_kwh, status, pause_reason, consecutive_failures,
	next_scheduled_at, next_poll_at, last_low_balance_triggered_at,
	last_reading_milli_kwh, last_reading_at, lease_until, created_at, updated_at`

func intsToInt32s(in []int) []int32 {
	if len(in) == 0 {
		return []int32{}
	}
	out := make([]int32, len(in))
	for i, v := range in {
		out[i] = int32(v)
	}
	return out
}

func int32sToInts(in []int32) []int {
	if len(in) == 0 {
		return nil
	}
	out := make([]int, len(in))
	for i, v := range in {
		out[i] = int(v)
	}
	return out
}

func utcTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func scanRule(row pgx.Row) (*autopower.Rule, error) {
	var rl autopower.Rule
	var trig, freq, st string
	var dow, dom []int32
	var endsAt, nextSched, nextPoll, lastTrig, lastReadAt *time.Time
	var lastReadMilli *int64
	if err := row.Scan(
		&rl.ID, &rl.UserID, &rl.MeterID, &rl.PaymentMethodID, &trig,
		&rl.AmountMinor, &rl.MonthlyCapMinor, &rl.Currency, &rl.DeliveryPreference,
		&freq, &dow, &dom, &rl.TimeOfDay, &rl.Timezone, &endsAt,
		&rl.LowBalanceMilliKWh, &st, &rl.PauseReason, &rl.ConsecutiveFailures,
		&nextSched, &nextPoll, &lastTrig,
		&lastReadMilli, &lastReadAt, &rl.LeaseUntil, &rl.CreatedAt, &rl.UpdatedAt,
	); err != nil {
		return nil, mapErr(err)
	}
	rl.TriggerType = autopower.TriggerType(trig)
	rl.Frequency = autopower.Frequency(freq)
	rl.Status = autopower.RuleStatus(st)
	rl.DaysOfWeek = int32sToInts(dow)
	rl.DaysOfMonth = int32sToInts(dom)
	rl.EndsAt = utcTimePtr(endsAt)
	rl.NextScheduledAt = utcTimePtr(nextSched)
	rl.NextPollAt = utcTimePtr(nextPoll)
	rl.LastLowBalanceTriggered = utcTimePtr(lastTrig)
	rl.LastReadingMilliKWh = lastReadMilli
	rl.LastReadingAt = utcTimePtr(lastReadAt)
	rl.LeaseUntil = rl.LeaseUntil.UTC()
	rl.CreatedAt = rl.CreatedAt.UTC()
	rl.UpdatedAt = rl.UpdatedAt.UTC()
	return &rl, nil
}

func (r autoPowerRepo) CreateRule(ctx context.Context, rl *autopower.Rule) error {
	uid, err := uuid.Parse(rl.UserID)
	if err != nil {
		return store.ErrNotFound
	}
	if rl.ID == uuid.Nil {
		rl.ID = uuid.New()
	}
	now := time.Now().UTC()
	if rl.CreatedAt.IsZero() {
		rl.CreatedAt = now
	} else {
		rl.CreatedAt = rl.CreatedAt.UTC()
	}
	if rl.UpdatedAt.IsZero() {
		rl.UpdatedAt = rl.CreatedAt
	} else {
		rl.UpdatedAt = rl.UpdatedAt.UTC()
	}
	if strings.TrimSpace(rl.Currency) == "" {
		rl.Currency = "NGN"
	}
	if strings.TrimSpace(rl.Timezone) == "" {
		rl.Timezone = autopower.DefaultTimezone
	}
	if strings.TrimSpace(rl.DeliveryPreference) == "" {
		rl.DeliveryPreference = "AUTO"
	}
	lease := rl.LeaseUntil.UTC()
	if rl.LeaseUntil.IsZero() {
		lease = time.Unix(0, 0).UTC()
	}
	rl.LeaseUntil = lease

	_, err = r.db.Exec(ctx, `
		INSERT INTO autopower_rules (
			id, user_id, meter_id, payment_method_id, trigger_type,
			amount_minor, monthly_cap_minor, currency, delivery_preference,
			frequency, days_of_week, days_of_month, time_of_day, timezone, ends_at,
			low_balance_milli_kwh, status, pause_reason, consecutive_failures,
			next_scheduled_at, next_poll_at, last_low_balance_triggered_at,
			last_reading_milli_kwh, last_reading_at, lease_until, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27)`,
		rl.ID, uid, rl.MeterID, rl.PaymentMethodID, string(rl.TriggerType),
		rl.AmountMinor, rl.MonthlyCapMinor, rl.Currency, rl.DeliveryPreference,
		string(rl.Frequency), intsToInt32s(rl.DaysOfWeek), intsToInt32s(rl.DaysOfMonth), rl.TimeOfDay, rl.Timezone, utcTimePtr(rl.EndsAt),
		rl.LowBalanceMilliKWh, string(rl.Status), rl.PauseReason, rl.ConsecutiveFailures,
		utcTimePtr(rl.NextScheduledAt), utcTimePtr(rl.NextPollAt), utcTimePtr(rl.LastLowBalanceTriggered),
		rl.LastReadingMilliKWh, utcTimePtr(rl.LastReadingAt), rl.LeaseUntil, rl.CreatedAt, rl.UpdatedAt,
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	return mapErr(err)
}

func (r autoPowerRepo) GetRule(ctx context.Context, id uuid.UUID) (*autopower.Rule, error) {
	return scanRule(r.db.QueryRow(ctx, `SELECT `+ruleCols+` FROM autopower_rules WHERE id = $1`, id))
}

func (r autoPowerRepo) GetRuleForUpdate(ctx context.Context, id uuid.UUID) (*autopower.Rule, error) {
	if !r.inTx {
		return nil, errors.New("autopower rule lock requires transaction")
	}
	return scanRule(r.db.QueryRow(ctx, `SELECT `+ruleCols+` FROM autopower_rules WHERE id = $1 FOR UPDATE`, id))
}

func (r autoPowerRepo) UpdateRule(ctx context.Context, rl *autopower.Rule) error {
	lease := rl.LeaseUntil.UTC()
	if rl.LeaseUntil.IsZero() {
		lease = time.Unix(0, 0).UTC()
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE autopower_rules SET
			payment_method_id             = $2,
			trigger_type                  = $3,
			amount_minor                  = $4,
			monthly_cap_minor             = $5,
			delivery_preference           = $6,
			frequency                     = $7,
			days_of_week                  = $8,
			days_of_month                 = $9,
			time_of_day                   = $10,
			timezone                      = $11,
			ends_at                       = $12,
			low_balance_milli_kwh         = $13,
			status                        = $14,
			pause_reason                  = $15,
			consecutive_failures          = $16,
			next_scheduled_at             = $17,
			next_poll_at                  = $18,
			last_low_balance_triggered_at = $19,
			last_reading_milli_kwh        = $20,
			last_reading_at               = $21,
			lease_until                   = $22,
			updated_at                    = $23
		WHERE id = $1`,
		rl.ID, rl.PaymentMethodID, string(rl.TriggerType),
		rl.AmountMinor, rl.MonthlyCapMinor, rl.DeliveryPreference,
		string(rl.Frequency), intsToInt32s(rl.DaysOfWeek), intsToInt32s(rl.DaysOfMonth),
		rl.TimeOfDay, rl.Timezone, utcTimePtr(rl.EndsAt),
		rl.LowBalanceMilliKWh, string(rl.Status), rl.PauseReason, rl.ConsecutiveFailures,
		utcTimePtr(rl.NextScheduledAt), utcTimePtr(rl.NextPollAt), utcTimePtr(rl.LastLowBalanceTriggered),
		rl.LastReadingMilliKWh, utcTimePtr(rl.LastReadingAt), lease, rl.UpdatedAt.UTC(),
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r autoPowerRepo) ListRulesForUser(ctx context.Context, userID string) ([]*autopower.Rule, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return []*autopower.Rule{}, nil
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+ruleCols+` FROM autopower_rules
		WHERE user_id = $1 AND status <> 'CANCELLED'
		ORDER BY created_at DESC, id ASC`, uid)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*autopower.Rule
	for rows.Next() {
		rl, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rl)
	}
	return out, rows.Err()
}

func (r autoPowerRepo) ListRulesByPaymentMethodForUpdate(ctx context.Context, paymentMethodID uuid.UUID) ([]*autopower.Rule, error) {
	if !r.inTx {
		return nil, errors.New("autopower rule lock requires transaction")
	}
	rows, err := r.db.Query(ctx, `
		SELECT `+ruleCols+` FROM autopower_rules
		WHERE payment_method_id = $1 AND status <> 'CANCELLED'
		ORDER BY id ASC
		FOR UPDATE`, paymentMethodID)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*autopower.Rule
	for rows.Next() {
		rl, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rl)
	}
	return out, rows.Err()
}

func (r autoPowerRepo) ClaimDueRulesForMaterialize(ctx context.Context, horizon, now time.Time, lease time.Duration, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 50
	}
	var ids []uuid.UUID
	err := r.atomic(ctx, func(db DB) error {
		rows, err := db.Query(ctx, `
			WITH due AS (
				SELECT id FROM autopower_rules
				WHERE status = 'ACTIVE'
				  AND next_scheduled_at IS NOT NULL
				  AND next_scheduled_at <= $1
				  AND lease_until <= $2
				ORDER BY next_scheduled_at ASC, id ASC
				LIMIT $4
				FOR UPDATE SKIP LOCKED
			)
			UPDATE autopower_rules rl
			SET lease_until = $3
			FROM due
			WHERE rl.id = due.id
			RETURNING rl.id`,
			horizon.UTC(), now.UTC(), now.UTC().Add(lease), limit)
		if err != nil {
			return mapErr(err)
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	return ids, err
}

func (r autoPowerRepo) ClaimDueLowBalanceRules(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 50
	}
	var ids []uuid.UUID
	err := r.atomic(ctx, func(db DB) error {
		rows, err := db.Query(ctx, `
			WITH due AS (
				SELECT id FROM autopower_rules
				WHERE status = 'ACTIVE'
				  AND trigger_type IN ('LOW_BALANCE', 'BOTH')
				  AND next_poll_at IS NOT NULL
				  AND next_poll_at <= $1
				  AND lease_until <= $1
				ORDER BY next_poll_at ASC, id ASC
				LIMIT $3
				FOR UPDATE SKIP LOCKED
			)
			UPDATE autopower_rules rl
			SET lease_until = $2
			FROM due
			WHERE rl.id = due.id
			RETURNING rl.id`,
			now.UTC(), now.UTC().Add(lease), limit)
		if err != nil {
			return mapErr(err)
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	return ids, err
}

// ---- autopower_runs --------------------------------------------------------

const runCols = `id, rule_id, user_id::text, meter_id, trigger_source,
	scheduled_for, executed_at, amount_minor, total_minor, trim(currency),
	status, heads_up_sent_at, transaction_id, payment_provider_ref,
	charge_dispatched_at, failure_reason, completed_at, lease_until,
	created_at, updated_at`

func scanRun(row pgx.Row) (*autopower.Run, error) {
	var rn autopower.Run
	var src, st string
	var execAt, headsUpAt, dispAt, compAt *time.Time
	var txnID *uuid.UUID
	if err := row.Scan(
		&rn.ID, &rn.RuleID, &rn.UserID, &rn.MeterID, &src,
		&rn.ScheduledFor, &execAt, &rn.AmountMinor, &rn.TotalMinor, &rn.Currency,
		&st, &headsUpAt, &txnID, &rn.PaymentProviderRef,
		&dispAt, &rn.FailureReason, &compAt, &rn.LeaseUntil,
		&rn.CreatedAt, &rn.UpdatedAt,
	); err != nil {
		return nil, mapErr(err)
	}
	rn.TriggerSource = autopower.TriggerSource(src)
	rn.Status = autopower.RunStatus(st)
	rn.ScheduledFor = rn.ScheduledFor.UTC()
	rn.ExecutedAt = utcTimePtr(execAt)
	rn.HeadsUpSentAt = utcTimePtr(headsUpAt)
	rn.TransactionID = txnID
	rn.ChargeDispatchedAt = utcTimePtr(dispAt)
	rn.CompletedAt = utcTimePtr(compAt)
	rn.LeaseUntil = rn.LeaseUntil.UTC()
	rn.CreatedAt = rn.CreatedAt.UTC()
	rn.UpdatedAt = rn.UpdatedAt.UTC()
	return &rn, nil
}

func (r autoPowerRepo) CreateRun(ctx context.Context, rn *autopower.Run) error {
	uid, err := uuid.Parse(rn.UserID)
	if err != nil {
		return store.ErrNotFound
	}
	if rn.ID == uuid.Nil {
		rn.ID = uuid.New()
	}
	now := time.Now().UTC()
	if rn.CreatedAt.IsZero() {
		rn.CreatedAt = now
	} else {
		rn.CreatedAt = rn.CreatedAt.UTC()
	}
	if rn.UpdatedAt.IsZero() {
		rn.UpdatedAt = rn.CreatedAt
	} else {
		rn.UpdatedAt = rn.UpdatedAt.UTC()
	}
	if strings.TrimSpace(rn.Currency) == "" {
		rn.Currency = "NGN"
	}
	rn.ScheduledFor = rn.ScheduledFor.UTC()
	lease := rn.LeaseUntil.UTC()
	if rn.LeaseUntil.IsZero() {
		lease = time.Unix(0, 0).UTC()
	}
	rn.LeaseUntil = lease

	_, err = r.db.Exec(ctx, `
		INSERT INTO autopower_runs (
			id, rule_id, user_id, meter_id, trigger_source,
			scheduled_for, executed_at, amount_minor, total_minor, currency,
			status, heads_up_sent_at, transaction_id, payment_provider_ref,
			charge_dispatched_at, failure_reason, completed_at, lease_until,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		rn.ID, rn.RuleID, uid, rn.MeterID, string(rn.TriggerSource),
		rn.ScheduledFor, utcTimePtr(rn.ExecutedAt), rn.AmountMinor, rn.TotalMinor, rn.Currency,
		string(rn.Status), utcTimePtr(rn.HeadsUpSentAt), rn.TransactionID, rn.PaymentProviderRef,
		utcTimePtr(rn.ChargeDispatchedAt), rn.FailureReason, utcTimePtr(rn.CompletedAt), rn.LeaseUntil,
		rn.CreatedAt, rn.UpdatedAt,
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	return mapErr(err)
}

func (r autoPowerRepo) GetRun(ctx context.Context, id uuid.UUID) (*autopower.Run, error) {
	return scanRun(r.db.QueryRow(ctx, `SELECT `+runCols+` FROM autopower_runs WHERE id = $1`, id))
}

func (r autoPowerRepo) GetRunForUpdate(ctx context.Context, id uuid.UUID) (*autopower.Run, error) {
	if !r.inTx {
		return nil, errors.New("autopower run lock requires transaction")
	}
	return scanRun(r.db.QueryRow(ctx, `SELECT `+runCols+` FROM autopower_runs WHERE id = $1 FOR UPDATE`, id))
}

func (r autoPowerRepo) GetRunBySlot(ctx context.Context, ruleID uuid.UUID, scheduledFor time.Time) (*autopower.Run, error) {
	return scanRun(r.db.QueryRow(ctx, `
		SELECT `+runCols+` FROM autopower_runs
		WHERE rule_id = $1 AND scheduled_for = $2`, ruleID, scheduledFor.UTC()))
}

func (r autoPowerRepo) GetRunByTxn(ctx context.Context, txnID uuid.UUID) (*autopower.Run, error) {
	return scanRun(r.db.QueryRow(ctx, `
		SELECT `+runCols+` FROM autopower_runs
		WHERE transaction_id = $1`, txnID))
}

func (r autoPowerRepo) UpdateRun(ctx context.Context, rn *autopower.Run) error {
	lease := rn.LeaseUntil.UTC()
	if rn.LeaseUntil.IsZero() {
		lease = time.Unix(0, 0).UTC()
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE autopower_runs SET
			executed_at          = $2,
			total_minor          = $3,
			status               = $4,
			heads_up_sent_at     = $5,
			transaction_id       = $6,
			payment_provider_ref = $7,
			charge_dispatched_at = $8,
			failure_reason       = $9,
			completed_at         = $10,
			lease_until          = $11,
			updated_at           = $12
		WHERE id = $1`,
		rn.ID, utcTimePtr(rn.ExecutedAt), rn.TotalMinor, string(rn.Status),
		utcTimePtr(rn.HeadsUpSentAt), rn.TransactionID, rn.PaymentProviderRef,
		utcTimePtr(rn.ChargeDispatchedAt), rn.FailureReason, utcTimePtr(rn.CompletedAt),
		lease, rn.UpdatedAt.UTC(),
	)
	if isFKViolation(err) {
		return store.ErrNotFound
	}
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r autoPowerRepo) ListRunsForRule(ctx context.Context, ruleID uuid.UUID, limit int) ([]*autopower.Run, error) {
	var rows pgx.Rows
	var err error
	if limit > 0 {
		rows, err = r.db.Query(ctx, `
			SELECT `+runCols+` FROM autopower_runs
			WHERE rule_id = $1 ORDER BY scheduled_for DESC, id ASC LIMIT $2`, ruleID, limit)
	} else {
		rows, err = r.db.Query(ctx, `
			SELECT `+runCols+` FROM autopower_runs
			WHERE rule_id = $1 ORDER BY scheduled_for DESC, id ASC`, ruleID)
	}
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []*autopower.Run
	for rows.Next() {
		rn, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rn)
	}
	return out, rows.Err()
}

func (r autoPowerRepo) CancelScheduledRunsForRule(ctx context.Context, ruleID uuid.UUID, now time.Time) error {
	ts := now.UTC()
	_, err := r.db.Exec(ctx, `
		UPDATE autopower_runs
		SET status = 'CANCELLED', completed_at = $2, updated_at = $2
		WHERE rule_id = $1 AND status = 'SCHEDULED'`, ruleID, ts)
	return mapErr(err)
}

func (r autoPowerRepo) ClaimDueRuns(ctx context.Context, headsUpHorizon, now, staleChargingBefore time.Time, lease time.Duration, limit int) ([]uuid.UUID, error) {
	if limit <= 0 {
		limit = 50
	}
	var ids []uuid.UUID
	err := r.atomic(ctx, func(db DB) error {
		rows, err := db.Query(ctx, `
			WITH due AS (
				SELECT id FROM autopower_runs
				WHERE lease_until <= $2
				  AND (
				        (status = 'SCHEDULED' AND heads_up_sent_at IS NULL AND scheduled_for <= $1)
				     OR (status = 'SCHEDULED' AND scheduled_for <= $2)
				     OR (status = 'CHARGING'  AND updated_at <= $3)
				  )
				ORDER BY scheduled_for ASC, id ASC
				LIMIT $5
				FOR UPDATE SKIP LOCKED
			)
			UPDATE autopower_runs rn
			SET lease_until = $4
			FROM due
			WHERE rn.id = due.id
			RETURNING rn.id`,
			headsUpHorizon.UTC(), now.UTC(), staleChargingBefore.UTC(), now.UTC().Add(lease), limit)
		if err != nil {
			return mapErr(err)
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		return err
	})
	return ids, err
}

func (r autoPowerRepo) SumRuleSpendMinor(ctx context.Context, ruleID uuid.UUID, startUTC, endUTC time.Time) (int64, error) {
	var sum int64
	err := r.db.QueryRow(ctx, `
		SELECT coalesce(SUM(amount_minor), 0)
		FROM autopower_runs
		WHERE rule_id = $1
		  AND status IN ('CHARGING', 'SUCCEEDED')
		  AND coalesce(executed_at, scheduled_for) >= $2
		  AND coalesce(executed_at, scheduled_for) < $3`,
		ruleID, startUTC.UTC(), endUTC.UTC()).Scan(&sum)
	return sum, mapErr(err)
}

func (r autoPowerRepo) SumUserSpendMinor(ctx context.Context, userID string, startUTC, endUTC time.Time) (int64, error) {
	uid, err := uuid.Parse(userID)
	if err != nil {
		return 0, nil
	}
	var sum int64
	err = r.db.QueryRow(ctx, `
		SELECT coalesce(SUM(amount_minor), 0)
		FROM autopower_runs
		WHERE user_id = $1
		  AND status IN ('CHARGING', 'SUCCEEDED')
		  AND coalesce(executed_at, scheduled_for) >= $2
		  AND coalesce(executed_at, scheduled_for) < $3`,
		uid, startUTC.UTC(), endUTC.UTC()).Scan(&sum)
	return sum, mapErr(err)
}
