CREATE SCHEMA IF NOT EXISTS groups;
CREATE TABLE IF NOT EXISTS groups.records (
 bucket text NOT NULL, id text NOT NULL, data jsonb NOT NULL,
 query_changed_at timestamptz NOT NULL DEFAULT now(),
 query_id uuid NOT NULL DEFAULT gen_random_uuid(),
 PRIMARY KEY(bucket,id)
);
CREATE TABLE IF NOT EXISTS groups.inbox (
 source text NOT NULL, id text NOT NULL, digest text NOT NULL,
 PRIMARY KEY(source,id)
);
CREATE TABLE IF NOT EXISTS groups.revisions (
 source text NOT NULL, subject text NOT NULL, revision bigint NOT NULL,
 PRIMARY KEY(source,subject)
);
CREATE TABLE IF NOT EXISTS groups.outbox (
 sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 id text UNIQUE NOT NULL, source text NOT NULL, subject text NOT NULL,
 envelope jsonb NOT NULL, created_at timestamptz NOT NULL,
 sent_at timestamptz, attempts integer NOT NULL DEFAULT 0,
 next_attempt timestamptz NOT NULL DEFAULT now(), held boolean NOT NULL DEFAULT false
);
CREATE INDEX IF NOT EXISTS groups_outbox_pending ON groups.outbox(sequence) WHERE sent_at IS NULL;

CREATE INDEX IF NOT EXISTS groups_pending_actions ON groups.records(bucket,id) WHERE data->>'state' IN ('pending','executing','unknown');

CREATE INDEX IF NOT EXISTS groups_records_bytewise ON groups.records(bucket,id COLLATE "C");

-- Relay excludes subjects before LIMIT; the partial index avoids retained history.
CREATE INDEX IF NOT EXISTS groups_outbox_delivery_pending ON groups.outbox
 (subject COLLATE "C",((envelope->>'revision')::bigint),id COLLATE "C") WHERE sent_at IS NULL;
-- Reverse membership lookup uses the canonical owner key, not a full group scan.
CREATE INDEX IF NOT EXISTS groups_members_by_owner ON groups.records(id,bucket)
 WHERE right(bucket,8)='/members';

CREATE UNIQUE INDEX IF NOT EXISTS groups_account_query_id ON groups.records(query_id);
