-- An interaction belongs to a sign-in session instead of a user, and the ones in progress can't be assigned to one
DELETE FROM interaction_sessions;
ALTER TABLE interaction_sessions DROP COLUMN user_id;
ALTER TABLE interaction_sessions ADD COLUMN session_id text;
