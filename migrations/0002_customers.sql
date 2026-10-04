CREATE TABLE quotagate.customers (
    id text PRIMARY KEY,
    name text NOT NULL,
    api_key_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    disabled_at timestamptz,
    CONSTRAINT customers_name_length CHECK (length(name) BETWEEN 1 AND 100),
    CONSTRAINT customers_key_hash_length CHECK (octet_length(api_key_hash) = 32)
);

CREATE TABLE quotagate.policies (
    customer_id text NOT NULL REFERENCES quotagate.customers(id) ON DELETE CASCADE,
    operation text NOT NULL,
    daily_limit bigint NOT NULL CHECK (daily_limit > 0),
    rate_limit_per_minute integer NOT NULL CHECK (rate_limit_per_minute > 0),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (customer_id, operation)
);
