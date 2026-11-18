CREATE SCHEMA IF NOT EXISTS groups;
CREATE TABLE IF NOT EXISTS groups.records (
 bucket text NOT NULL, id text NOT NULL, data jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
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
ALTER TABLE groups.outbox ADD COLUMN IF NOT EXISTS first_failed_at timestamptz;
CREATE INDEX IF NOT EXISTS groups_outbox_pending ON groups.outbox(sequence) WHERE sent_at IS NULL;
CREATE INDEX IF NOT EXISTS groups_outbox_sent_at ON groups.outbox(sent_at,sequence) WHERE sent_at IS NOT NULL;

CREATE INDEX IF NOT EXISTS groups_pending_actions ON groups.records(bucket,id) WHERE data->>'state' IN ('pending','executing','unknown');

CREATE INDEX IF NOT EXISTS groups_records_bytewise ON groups.records(bucket,id COLLATE "C");
-- Earlier records have a last-change time; use their original event time when present.
DO $$ BEGIN
 IF NOT EXISTS (SELECT 1 FROM pg_attribute WHERE attrelid='groups.records'::regclass AND attname='created_at' AND NOT attisdropped) THEN
  ALTER TABLE groups.records ADD COLUMN created_at timestamptz;
  UPDATE groups.records SET created_at=CASE WHEN pg_input_is_valid(data->>'created','timestamptz') THEN (data->>'created')::timestamptz ELSE query_changed_at END;
  ALTER TABLE groups.records ALTER COLUMN created_at SET DEFAULT clock_timestamp(), ALTER COLUMN created_at SET NOT NULL;
 END IF;
END $$;
CREATE INDEX IF NOT EXISTS groups_records_created_desc ON groups.records(bucket,created_at DESC,id COLLATE "C" DESC);
CREATE INDEX IF NOT EXISTS groups_members_state ON groups.records(bucket,(data->>'state'),id COLLATE "C") WHERE right(bucket,8)='/members';
CREATE INDEX IF NOT EXISTS groups_actions_state_kind ON groups.records(bucket,(data->>'state'),(data->'command'->>'kind'),id COLLATE "C") WHERE right(bucket,8)='/actions';
CREATE INDEX IF NOT EXISTS groups_reports_state ON groups.records(bucket,(data->>'state'),id COLLATE "C") WHERE right(bucket,8)='/reports';
CREATE INDEX IF NOT EXISTS groups_messages_recalled ON groups.records(bucket,id COLLATE "C") WHERE right(bucket,9)='/messages' AND data->>'recalled'='true';

-- Relay excludes subjects before LIMIT; the partial index avoids retained history.
CREATE INDEX IF NOT EXISTS groups_outbox_delivery_pending ON groups.outbox
 (subject COLLATE "C",((envelope->>'revision')::bigint),id COLLATE "C") WHERE sent_at IS NULL;
-- Reverse membership lookup uses the canonical owner key, not a full group scan.
CREATE INDEX IF NOT EXISTS groups_members_by_owner ON groups.records(id,bucket)
 WHERE right(bucket,8)='/members';

-- Student directory lookup scans only groups explicitly marked discoverable.
CREATE INDEX IF NOT EXISTS groups_discoverable_config ON groups.records(id COLLATE "C")
 WHERE bucket='config' AND data->>'discoverable'='true';

CREATE UNIQUE INDEX IF NOT EXISTS groups_account_query_id ON groups.records(query_id);

CREATE INDEX IF NOT EXISTS groups_account_subject ON groups.records(id)
 WHERE split_part(bucket,'/',2) IN ('members','blacklist','warnings','watch');

CREATE INDEX IF NOT EXISTS groups_action_target ON groups.records((data->'command'->>'target'))
 WHERE split_part(bucket,'/',2) ='actions';

CREATE INDEX IF NOT EXISTS groups_audit_target ON groups.records((data->>'target'))
 WHERE split_part(bucket,'/',2) ='audit';

CREATE INDEX IF NOT EXISTS groups_audit_actor ON groups.records((data->>'actor'))
 WHERE split_part(bucket,'/',2) ='audit';

CREATE INDEX IF NOT EXISTS groups_actor_identity ON groups.records((data->'actor'->>'issuer'),(data->'actor'->>'subject'))
 WHERE split_part(bucket,'/',2) IN ('actions','audit');
