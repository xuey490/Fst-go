CREATE TABLE IF NOT EXISTS peers (
  peer_id    TEXT PRIMARY KEY,
  name       TEXT NOT NULL DEFAULT '',
  platform   TEXT NOT NULL DEFAULT '',
  last_addrs TEXT NOT NULL DEFAULT '',
  first_seen INTEGER NOT NULL,
  last_seen  INTEGER NOT NULL,
  trusted    INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS messages (
  id        TEXT PRIMARY KEY,
  peer_id   TEXT NOT NULL,
  direction TEXT NOT NULL,
  content   TEXT NOT NULL,
  status    TEXT NOT NULL,
  ts        INTEGER NOT NULL,
  read      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_messages_peer_ts ON messages (peer_id, ts);

CREATE TABLE IF NOT EXISTS transfers (
  id          TEXT PRIMARY KEY,
  batch_id    TEXT NOT NULL,
  peer_id     TEXT NOT NULL,
  direction   TEXT NOT NULL,
  file_name   TEXT NOT NULL,
  size        INTEGER NOT NULL,
  sha256      TEXT NOT NULL DEFAULT '',
  local_path  TEXT NOT NULL DEFAULT '',
  status      TEXT NOT NULL,
  bytes_done  INTEGER NOT NULL DEFAULT 0,
  error       TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  finished_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_transfers_batch ON transfers (batch_id);
CREATE INDEX IF NOT EXISTS idx_transfers_created ON transfers (created_at);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
