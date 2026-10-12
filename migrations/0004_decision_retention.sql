-- Retention deletes by age across all customers; the existing index leads with customer_id.
CREATE INDEX decisions_created_at_idx ON quotagate.decisions (created_at);
