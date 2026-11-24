-- Accounts that share an address apart from its letter case can't all keep it, so it stays with the verified one, or else with the oldest
UPDATE users
SET email          = NULL,
    email_verified = FALSE,
    updated_at     = now()
WHERE id IN (SELECT id
             FROM (SELECT id, row_number() OVER (PARTITION BY lower(email COLLATE "C") ORDER BY email_verified DESC, created_at, id) AS position
                   FROM users
                   WHERE email IS NOT NULL) ranked
             WHERE position > 1);

-- Compare the email like the username
ALTER TABLE users ALTER COLUMN email TYPE citext COLLATE "C";
