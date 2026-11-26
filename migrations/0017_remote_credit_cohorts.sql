-- Operator-attested AMI / remote-credit cohorts.
-- Production must not infer remote eligibility from a meter-number prefix in
-- application code. A cohort is the written matrix from a MAP/DisCo/HES pack:
-- "IKEDC meters starting with 62 are Hexing AMI". Live Authorised still requires
-- a configured RemoteCreditProvider and a successful CheckEligibility.

CREATE TABLE remote_credit_cohorts (
    id             UUID PRIMARY KEY,
    disco_code     TEXT NOT NULL,
    number_prefix  TEXT NOT NULL,
    manufacturer   TEXT NOT NULL DEFAULT '',
    authorised     BOOLEAN NOT NULL DEFAULT true,
    evidence       TEXT NOT NULL,
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at     TIMESTAMPTZ,
    CONSTRAINT remote_credit_cohorts_prefix_digits
        CHECK (number_prefix ~ '^[0-9]{1,13}$'),
    CONSTRAINT remote_credit_cohorts_disco
        CHECK (disco_code ~ '^[A-Z0-9]{2,16}$'),
    CONSTRAINT remote_credit_cohorts_evidence
        CHECK (char_length(evidence) BETWEEN 5 AND 500)
);

CREATE UNIQUE INDEX remote_credit_cohorts_active_uq
    ON remote_credit_cohorts (disco_code, number_prefix)
    WHERE revoked_at IS NULL;

CREATE INDEX remote_credit_cohorts_disco_idx
    ON remote_credit_cohorts (disco_code)
    WHERE revoked_at IS NULL;
