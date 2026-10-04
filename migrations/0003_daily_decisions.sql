CREATE TABLE quotagate.daily_usage (
    customer_id text NOT NULL,
    operation text NOT NULL,
    utc_day date NOT NULL,
    used bigint NOT NULL CHECK (used >= 0),
    PRIMARY KEY (customer_id, operation, utc_day),
    FOREIGN KEY (customer_id, operation)
        REFERENCES quotagate.policies(customer_id, operation) ON DELETE CASCADE
);

CREATE TABLE quotagate.decisions (
    customer_id text NOT NULL,
    decision_id text NOT NULL,
    operation text NOT NULL,
    utc_day date NOT NULL,
    allowed boolean NOT NULL,
    reason text NOT NULL CHECK (reason IN ('allowed', 'daily_quota', 'rate_limited')),
    daily_used bigint NOT NULL CHECK (daily_used >= 0),
    daily_limit bigint NOT NULL CHECK (daily_limit > 0),
    policy_version bigint NOT NULL CHECK (policy_version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (customer_id, decision_id),
    FOREIGN KEY (customer_id, operation)
        REFERENCES quotagate.policies(customer_id, operation) ON DELETE CASCADE,
    CONSTRAINT decision_reason_matches_allowed CHECK (allowed = (reason = 'allowed'))
);

CREATE INDEX decisions_customer_created_at_idx
    ON quotagate.decisions (customer_id, created_at);
