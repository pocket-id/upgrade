DROP INDEX idx_oauth2_sessions_kind_request;
CREATE INDEX idx_oauth2_sessions_kind_request ON oauth2_sessions (kind, request_id, active, rotated_at);
