PRAGMA foreign_keys=OFF;
BEGIN;

CREATE TABLE audit_logs_new
(
    id         TEXT NOT NULL PRIMARY KEY,
    created_at DATETIME NOT NULL,
    event      TEXT NOT NULL,
    ip_address TEXT,
    user_agent TEXT NOT NULL,
    data       BLOB NOT NULL,
    user_id    TEXT REFERENCES users ON DELETE SET NULL,
    country    TEXT,
    city       TEXT,
    actor_type TEXT,
    actor_id   TEXT,
    actor_name TEXT
);

-- Earlier entries were actions users took on their own account
INSERT INTO audit_logs_new (id, created_at, event, ip_address, user_agent, data, user_id, country, city, actor_type, actor_id, actor_name)
SELECT audit_logs.id, audit_logs.created_at, event, ip_address, user_agent, data, user_id, country, city,
       CASE WHEN users.id IS NOT NULL THEN 'user' END, users.id, users.username
FROM audit_logs
         LEFT JOIN users ON users.id = audit_logs.user_id;

-- The exception is an admin removing another user's passkey, who was recorded in the data of the entry
UPDATE audit_logs_new
SET actor_type = 'user',
    actor_id   = json_extract(data, '$.actorUserID'),
    actor_name = json_extract(data, '$.actorUsername')
WHERE json_extract(data, '$.actorUserID') IS NOT NULL;

DROP TABLE audit_logs;
ALTER TABLE audit_logs_new RENAME TO audit_logs;

CREATE INDEX idx_audit_logs_client_name ON audit_logs((json_extract(data, '$.clientName')));
CREATE INDEX idx_audit_logs_country ON audit_logs (country);
CREATE INDEX idx_audit_logs_created_at ON audit_logs (created_at);
CREATE INDEX idx_audit_logs_event ON audit_logs (event);
CREATE INDEX idx_audit_logs_user_agent ON audit_logs (user_agent);
CREATE INDEX idx_audit_logs_user_id ON audit_logs (user_id);
CREATE INDEX idx_audit_logs_actor_id ON audit_logs (actor_id);

COMMIT;
PRAGMA foreign_keys=ON;
