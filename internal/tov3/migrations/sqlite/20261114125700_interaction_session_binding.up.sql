-- An interaction belongs to a sign-in session instead of a user, and the ones in progress can't be assigned to one
DROP TABLE interaction_sessions;

CREATE TABLE "interaction_sessions" (
    id TEXT NOT NULL PRIMARY KEY,
    created_at DATETIME NOT NULL,
    consent_required BOOLEAN NOT NULL DEFAULT FALSE,
    reauthentication_required BOOLEAN NOT NULL DEFAULT FALSE,
    authentication_required BOOLEAN NOT NULL DEFAULT FALSE,
    account_selection_required BOOLEAN NOT NULL DEFAULT FALSE,
    scopes BLOB NOT NULL DEFAULT X'5B5D',
    client_id TEXT NOT NULL REFERENCES oidc_clients(id) ON DELETE CASCADE,
    requested_at DATETIME NOT NULL,
    reauthenticated_at DATETIME,
    parameters BLOB NOT NULL DEFAULT X'7B7D',
    session_id TEXT
);

CREATE INDEX idx_interaction_sessions_client_id ON interaction_sessions (client_id);
