-- 0034_search_budgets.sql — SG-01 durable futile-search budget.
-- Stores only a canonical external media identity and bounded counters/times.
-- No title, query, provider response, URL, credential, or release data enters
-- this table.

-- +goose Up

CREATE TABLE search_budgets (
    identity_key           TEXT PRIMARY KEY,
    consecutive_no_results INTEGER NOT NULL CHECK (consecutive_no_results >= 0),
    suppressed_until       TEXT,
    updated_at             TEXT NOT NULL
);

CREATE INDEX idx_search_budgets_suppressed_until
    ON search_budgets(suppressed_until);

-- +goose Down

DROP INDEX IF EXISTS idx_search_budgets_suppressed_until;
DROP TABLE IF EXISTS search_budgets;
