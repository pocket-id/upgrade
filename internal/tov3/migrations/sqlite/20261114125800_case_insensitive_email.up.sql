PRAGMA foreign_keys = OFF;
BEGIN;

-- Accounts that share an address apart from its letter case can't all keep it, so it stays with the verified one, or else with the oldest
UPDATE users
SET email          = NULL,
    email_verified = FALSE,
    updated_at     = unixepoch()
WHERE id IN (SELECT id
             FROM (SELECT id, row_number() OVER (PARTITION BY lower(email) ORDER BY email_verified DESC, created_at, id) AS position
                   FROM users
                   WHERE email IS NOT NULL)
             WHERE position > 1);

-- Compare the email like the username, which needs a new table because SQLite can't change the collation of a column
CREATE TABLE users_new
(
    id                          TEXT                  NOT NULL PRIMARY KEY,
    created_at                  DATETIME,
    updated_at                  DATETIME,
    username                    TEXT COLLATE NOCASE   NOT NULL UNIQUE,
    email                       TEXT COLLATE NOCASE UNIQUE,
    first_name                  TEXT                  NOT NULL,
    last_name                   TEXT                  NOT NULL,
    display_name                TEXT                  NOT NULL,
    is_admin                    BOOLEAN DEFAULT FALSE NOT NULL,
    ldap_id                     TEXT UNIQUE,
    locale                      TEXT,
    disabled                    BOOLEAN DEFAULT FALSE NOT NULL,
    email_verified              BOOLEAN DEFAULT FALSE NOT NULL,
    email_verification_deadline DATETIME
);

INSERT INTO users_new (id, created_at, updated_at, username, email, first_name, last_name, display_name, is_admin, ldap_id, locale, disabled, email_verified, email_verification_deadline)
SELECT id, created_at, updated_at, username, email, first_name, last_name, display_name, is_admin, ldap_id, locale, disabled, email_verified, email_verification_deadline
FROM users;

DROP TABLE users;
ALTER TABLE users_new RENAME TO users;

COMMIT;
PRAGMA foreign_keys = ON;
