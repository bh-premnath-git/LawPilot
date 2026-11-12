-- Existing databases: allow the API to atomically append metadata-only execution events.
GRANT INSERT ON outbox_events TO lawpilot_app;
