-- 0014: Encrypted support chat (Meterly Agent + E2E human operators).
-- Append-only. HUMAN message bodies are client-side ciphertext only.
-- AGENT/SYSTEM bodies are server-sealed (AES-GCM) so a DB dump is not plaintext.
-- Identity public keys are not secret; private keys never leave the device.

CREATE TABLE IF NOT EXISTS support_identity_keys (
    user_id     UUID        PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    public_key  BYTEA       NOT NULL,
    algorithm   TEXT        NOT NULL CHECK (algorithm IN ('P-256')),
    staff       BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    rotated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS support_identity_keys_staff_idx
    ON support_identity_keys (staff) WHERE staff;

CREATE TABLE IF NOT EXISTS support_threads (
    id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id           UUID        NOT NULL REFERENCES users(id),
    mode                  TEXT        NOT NULL CHECK (mode IN ('AGENT', 'HUMAN')),
    status                TEXT        NOT NULL CHECK (status IN (
        'OPEN', 'WAITING_HUMAN', 'ASSIGNED', 'RESOLVED', 'CLOSED'
    )),
    assigned_operator_id  UUID        REFERENCES users(id),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_message_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS support_threads_customer_idx
    ON support_threads (customer_id, last_message_at DESC);
CREATE INDEX IF NOT EXISTS support_threads_staff_idx
    ON support_threads (status, last_message_at DESC);

CREATE TABLE IF NOT EXISTS support_thread_keys (
    thread_id       UUID        NOT NULL REFERENCES support_threads(id) ON DELETE CASCADE,
    holder_user_id  UUID        NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    wrapped_key     BYTEA       NOT NULL,
    nonce           BYTEA       NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (thread_id, holder_user_id)
);

CREATE TABLE IF NOT EXISTS support_messages (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    thread_id    UUID        NOT NULL REFERENCES support_threads(id) ON DELETE CASCADE,
    sender_id    TEXT        NOT NULL,
    sender_kind  TEXT        NOT NULL CHECK (sender_kind IN ('CUSTOMER', 'OPERATOR', 'AGENT', 'SYSTEM')),
    ciphertext   BYTEA,
    nonce        BYTEA,
    body_enc     BYTEA,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (
        (ciphertext IS NOT NULL AND nonce IS NOT NULL AND body_enc IS NULL)
        OR
        (body_enc IS NOT NULL AND ciphertext IS NULL AND nonce IS NULL)
    )
);

CREATE INDEX IF NOT EXISTS support_messages_thread_idx
    ON support_messages (thread_id, created_at ASC, id ASC);
