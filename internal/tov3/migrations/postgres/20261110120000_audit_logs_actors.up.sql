ALTER TABLE audit_logs
    DROP CONSTRAINT IF EXISTS audit_logs_user_id_fkey,
    ADD CONSTRAINT audit_logs_user_id_fkey
        FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE SET NULL,
    ADD COLUMN actor_type TEXT,
    ADD COLUMN actor_id   TEXT,
    ADD COLUMN actor_name TEXT;

-- Earlier entries were actions users took on their own account
UPDATE audit_logs
SET actor_type = 'user',
    actor_id   = audit_logs.user_id::text,
    actor_name = users.username
FROM users
WHERE users.id = audit_logs.user_id;

-- The exception is an admin removing another user's passkey, who was recorded in the data of the entry
UPDATE audit_logs
SET actor_type = 'user',
    actor_id   = data ->> 'actorUserID',
    actor_name = data ->> 'actorUsername'
WHERE data ->> 'actorUserID' IS NOT NULL;

CREATE INDEX idx_audit_logs_actor_id ON audit_logs (actor_id);
