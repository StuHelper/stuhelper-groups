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

-- Candidate 0.2 records become the final 0.3 wire/storage shape once. Historical
-- unsalted password hashes cannot satisfy the new password format: administrators
-- must set a new password before password-mode admission can succeed.
UPDATE groups.records SET data=(data-'password_hash') || jsonb_build_object(
 'name',COALESCE(data->>'name',id),'number',COALESCE(data->>'number',''),
 'bot_enabled',COALESCE((data->>'bot_enabled')::boolean,true),
 'revision',COALESCE((data->>'revision')::bigint,1),
 'password_set',false,'password_digest','')
 WHERE bucket='config' AND (NOT data ? 'revision' OR data ? 'password_hash');
UPDATE groups.records SET data=jsonb_set(data,'{actor}',jsonb_build_object(
 'issuer',data->'actor'->>'Issuer','subject',data->'actor'->>'Subject'))
 WHERE (bucket LIKE '%/actions' OR bucket LIKE '%/subscriptions') AND data->'actor' ? 'Issuer';
UPDATE groups.records SET data=(data-'Reason'-'Until'-'Permanent') || jsonb_build_object(
 'reason',data->'Reason','until',data->'Until','permanent',data->'Permanent')
 WHERE bucket LIKE '%/blacklist' AND data ? 'Reason';
UPDATE groups.records SET data=(data-'Count') || jsonb_build_object('count',data->'Count')
 WHERE bucket LIKE '%/warnings' AND data ? 'Count';
CREATE INDEX IF NOT EXISTS groups_records_bytewise ON groups.records(bucket,id COLLATE "C");

UPDATE groups.records SET data=jsonb_set(data || jsonb_build_object('lease_until',data->'result'),'{result}','""'::jsonb)
 WHERE bucket LIKE '%/actions' AND data->>'state'='executing' AND NOT data ? 'lease_until';

-- Relay excludes subjects before LIMIT; the partial index avoids retained history.
CREATE INDEX IF NOT EXISTS groups_outbox_delivery_pending ON groups.outbox
 (subject COLLATE "C",((envelope->>'revision')::bigint),id COLLATE "C") WHERE sent_at IS NULL;
-- Reverse membership lookup uses the canonical owner key, not a full group scan.
CREATE INDEX IF NOT EXISTS groups_members_by_owner ON groups.records(id,bucket)
 WHERE right(bucket,8)='/members';

-- Track source changes for account queries; existing rows start at migration time.
CREATE OR REPLACE FUNCTION groups.account_query_changed() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.query_changed_at=clock_timestamp(); RETURN NEW; END $$;
ALTER TABLE groups.records ADD COLUMN IF NOT EXISTS query_changed_at timestamptz NOT NULL DEFAULT now();
CREATE OR REPLACE TRIGGER account_query_changed BEFORE UPDATE ON groups.records FOR EACH ROW EXECUTE FUNCTION groups.account_query_changed();
