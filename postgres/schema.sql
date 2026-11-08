CREATE SCHEMA IF NOT EXISTS groups;
CREATE TABLE IF NOT EXISTS groups.records (
 bucket text NOT NULL, id text NOT NULL, data jsonb NOT NULL,
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
