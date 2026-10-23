-- +cms expand
-- Form submissions (spec 4.8). Field values are personal data, so they are
-- stored encrypted (envelope, AAD = row id). Nothing about the sender's IP or
-- browser is kept; rate limiting happens in memory.
-- +goose Up
CREATE TABLE form_submissions (
    id         uuid PRIMARY KEY,
    form       text NOT NULL,
    data_enc   bytea NOT NULL,
    summary    text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now(),
    read_at    timestamptz
);
-- The table is new and empty, so a plain index build blocks nothing.
-- squawk-ignore require-concurrent-index-creation
CREATE INDEX form_submissions_form_created_idx ON form_submissions (form, created_at DESC);

-- +goose Down
DROP TABLE form_submissions;
