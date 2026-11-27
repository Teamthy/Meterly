package pgstore

import (
	"context"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/reconcile"
	"github.com/teamthy/meterly/internal/store"
)

type reconRepo struct{ *repos }

func scanReconRun(row pgx.Row) (reconcile.Run, error) {
	var r reconcile.Run
	var completed *time.Time
	err := row.Scan(&r.ID, &r.Provider, &r.BusinessDate, &r.Status, &r.StartedAt, &completed,
		&r.MatchedCount, &r.MatchedMinor, &r.ExceptionCount, &r.Error)
	if err != nil {
		return reconcile.Run{}, mapErr(err)
	}
	r.BusinessDate = reconcile.DateOnly(r.BusinessDate)
	r.StartedAt = r.StartedAt.UTC()
	if completed != nil {
		r.CompletedAt = completed.UTC()
	}
	return r, nil
}

func (r reconRepo) ClaimRun(ctx context.Context, provider string, date time.Time, id uuid.UUID) (reconcile.Run, error) {
	date = reconcile.DateOnly(date)
	return scanReconRun(r.db.QueryRow(ctx, `
		INSERT INTO reconciliation_runs (id, provider, business_date, status, started_at)
		VALUES ($1, $2, $3::date, 'RUNNING', now())
		ON CONFLICT (provider, business_date) DO UPDATE SET
			status = CASE WHEN reconciliation_runs.status = 'COMPLETED' THEN reconciliation_runs.status ELSE 'RUNNING' END,
			started_at = CASE WHEN reconciliation_runs.status = 'COMPLETED' THEN reconciliation_runs.started_at ELSE now() END,
			error = CASE WHEN reconciliation_runs.status = 'COMPLETED' THEN reconciliation_runs.error ELSE '' END
		RETURNING id, provider, business_date, status, started_at, completed_at,
		          matched_count, matched_minor, exception_count, error`,
		id, provider, date.Format("2006-01-02")))
}

func (r reconRepo) GetRun(ctx context.Context, provider string, date time.Time) (reconcile.Run, error) {
	return scanReconRun(r.db.QueryRow(ctx, `
		SELECT id, provider, business_date, status, started_at, completed_at,
		       matched_count, matched_minor, exception_count, error
		FROM reconciliation_runs WHERE provider = $1 AND business_date = $2::date`,
		provider, reconcile.DateOnly(date).Format("2006-01-02")))
}

func (r reconRepo) CompleteRun(ctx context.Context, run reconcile.Run) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE reconciliation_runs
		SET status = 'COMPLETED', completed_at = $2, matched_count = $3, matched_minor = $4,
		    exception_count = $5, error = ''
		WHERE id = $1`, run.ID, run.CompletedAt.UTC(), run.MatchedCount, run.MatchedMinor, run.ExceptionCount)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r reconRepo) FailRun(ctx context.Context, id uuid.UUID, errText string) error {
	tag, err := r.db.Exec(ctx, `UPDATE reconciliation_runs SET status = 'FAILED', error = $2 WHERE id = $1`, id, errText)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func scanEx(row pgx.Row) (reconcile.Exception, error) {
	var e reconcile.Exception
	var txn *uuid.UUID
	err := row.Scan(&e.ID, &e.RunID, &e.Provider, &e.BusinessDate, &e.Category, &e.Severity,
		&e.ProviderRef, &txn, &e.OursMinor, &e.TheirsMinor, &e.Currency, &e.Status, &e.Owner, &e.Notes,
		&e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return reconcile.Exception{}, mapErr(err)
	}
	if txn != nil {
		e.OurTxnID = *txn
	}
	e.BusinessDate = reconcile.DateOnly(e.BusinessDate)
	e.CreatedAt, e.UpdatedAt = e.CreatedAt.UTC(), e.UpdatedAt.UTC()
	return e, nil
}

func (r reconRepo) UpsertException(ctx context.Context, e *reconcile.Exception) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	var txn any
	if e.OurTxnID != uuid.Nil {
		txn = e.OurTxnID
	}
	fp := e.Fingerprint()
	err := r.db.QueryRow(ctx, `
		INSERT INTO reconciliation_exceptions (
			id, run_id, provider, business_date, category, severity, provider_ref,
			our_transaction_id, ours_minor, theirs_minor, currency, status, owner, notes, fingerprint)
		VALUES ($1,$2,$3,$4::date,$5,$6,$7,$8,$9,$10,$11,'OPEN','','',$12)
		ON CONFLICT (fingerprint) DO UPDATE SET
			run_id = EXCLUDED.run_id,
			ours_minor = EXCLUDED.ours_minor,
			theirs_minor = EXCLUDED.theirs_minor,
			updated_at = now()
		WHERE reconciliation_exceptions.status IN ('OPEN', 'INVESTIGATING')
		RETURNING id, status`,
		e.ID, e.RunID, e.Provider, reconcile.DateOnly(e.BusinessDate).Format("2006-01-02"),
		string(e.Category), string(e.Severity), e.ProviderRef, txn, e.OursMinor, e.TheirsMinor, e.Currency, fp,
	).Scan(&e.ID, &e.Status)
	if err != nil {
		existing, gerr := scanEx(r.db.QueryRow(ctx, `
			SELECT id, run_id, provider, business_date, category, severity, provider_ref,
			       our_transaction_id, ours_minor, theirs_minor, currency, status, owner, notes,
			       created_at, updated_at
			FROM reconciliation_exceptions WHERE fingerprint = $1`, fp))
		if gerr != nil {
			return mapErr(err)
		}
		*e = existing
		return nil
	}
	return nil
}

func (r reconRepo) GetException(ctx context.Context, id uuid.UUID) (reconcile.Exception, error) {
	return scanEx(r.db.QueryRow(ctx, `
		SELECT id, run_id, provider, business_date, category, severity, provider_ref,
		       our_transaction_id, ours_minor, theirs_minor, currency, status, owner, notes,
		       created_at, updated_at
		FROM reconciliation_exceptions WHERE id = $1`, id))
}

func (r reconRepo) ListExceptions(ctx context.Context, f reconcile.ExceptionFilter) ([]reconcile.Exception, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q := `
		SELECT id, run_id, provider, business_date, category, severity, provider_ref,
		       our_transaction_id, ours_minor, theirs_minor, currency, status, owner, notes,
		       created_at, updated_at
		FROM reconciliation_exceptions WHERE 1=1`
	args := []any{}
	n := 1
	if f.Provider != "" {
		q += ` AND provider = $` + strconv.Itoa(n)
		args = append(args, f.Provider)
		n++
	}
	if f.Status != "" {
		q += ` AND status = $` + strconv.Itoa(n)
		args = append(args, string(f.Status))
		n++
	}
	if f.Category != "" {
		q += ` AND category = $` + strconv.Itoa(n)
		args = append(args, string(f.Category))
		n++
	}
	if !f.BusinessDate.IsZero() {
		q += ` AND business_date = $` + strconv.Itoa(n) + `::date`
		args = append(args, reconcile.DateOnly(f.BusinessDate).Format("2006-01-02"))
		n++
	}
	q += ` ORDER BY created_at DESC LIMIT $` + strconv.Itoa(n)
	args = append(args, limit)
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []reconcile.Exception
	for rows.Next() {
		e, err := scanEx(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (r reconRepo) UpdateException(ctx context.Context, e reconcile.Exception) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE reconciliation_exceptions
		SET status = $2, owner = $3, notes = $4, updated_at = now()
		WHERE id = $1`, e.ID, string(e.Status), e.Owner, e.Notes)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (r reconRepo) CountOpen(ctx context.Context) ([]reconcile.OpenCount, error) {
	rows, err := r.db.Query(ctx, `
		SELECT provider, category, count(*)
		FROM reconciliation_exceptions
		WHERE status IN ('OPEN', 'INVESTIGATING')
		GROUP BY provider, category`)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []reconcile.OpenCount
	for rows.Next() {
		var c reconcile.OpenCount
		if err := rows.Scan(&c.Provider, &c.Category, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r reconRepo) CreateAdjustment(ctx context.Context, a *reconcile.Adjustment) error {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO reconciliation_adjustments (
			id, exception_id, proposed_by, status, debit_account, credit_account,
			amount_minor, currency, reason, created_at)
		VALUES ($1,$2,$3,'PROPOSED',$4,$5,$6,$7,$8,$9)`,
		a.ID, a.ExceptionID, a.ProposedBy, string(a.DebitAccount), string(a.CreditAccount),
		a.AmountMinor, a.Currency, a.Reason, a.CreatedAt.UTC())
	return mapErr(err)
}

func scanAdj(row pgx.Row) (reconcile.Adjustment, error) {
	var a reconcile.Adjustment
	var approvedBy *uuid.UUID
	var approvedAt *time.Time
	var debit, credit string
	err := row.Scan(&a.ID, &a.ExceptionID, &a.ProposedBy, &approvedBy, &a.Status, &debit, &credit,
		&a.AmountMinor, &a.Currency, &a.LedgerReference, &a.Reason, &a.CreatedAt, &approvedAt)
	if err != nil {
		return reconcile.Adjustment{}, mapErr(err)
	}
	a.DebitAccount, a.CreditAccount = ledger.AccountCode(debit), ledger.AccountCode(credit)
	if approvedBy != nil {
		a.ApprovedBy = approvedBy.String()
	}
	a.CreatedAt = a.CreatedAt.UTC()
	if approvedAt != nil {
		a.ApprovedAt = approvedAt.UTC()
	}
	return a, nil
}

func (r reconRepo) GetAdjustment(ctx context.Context, id uuid.UUID) (reconcile.Adjustment, error) {
	return scanAdj(r.db.QueryRow(ctx, `
		SELECT id, exception_id, proposed_by::text, approved_by, status, debit_account, credit_account,
		       amount_minor, currency, ledger_reference, reason, created_at, approved_at
		FROM reconciliation_adjustments WHERE id = $1`, id))
}

func (r reconRepo) UpdateAdjustment(ctx context.Context, a reconcile.Adjustment) error {
	var approvedBy any
	if a.ApprovedBy != "" {
		if id, err := uuid.Parse(a.ApprovedBy); err == nil {
			approvedBy = id
		}
	}
	var approvedAt any
	if !a.ApprovedAt.IsZero() {
		approvedAt = a.ApprovedAt.UTC()
	}
	tag, err := r.db.Exec(ctx, `
		UPDATE reconciliation_adjustments
		SET status = $2, approved_by = $3, approved_at = $4, ledger_reference = $5
		WHERE id = $1`, a.ID, string(a.Status), approvedBy, approvedAt, a.LedgerReference)
	if err != nil {
		return mapErr(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}
	return nil
}
