-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Copyright (C) 2026 LectureFlow contributors. Licensed under the GNU AGPL v3 or later; see LICENSE.
-- ssh-mvp (.claude/research/remote-ssh-plan.md §2.1; user decision
-- 2026-10-27, plan §10 item 3): the user's own SSH inference servers. A
-- new table; no table of §3.3 changes and the asr_device CHECK stays: a
-- server's device row is "ssh:<id>" with the engine it runs as backend.
-- No secret is stored here: key_ref names the kv row holding the private
-- key, sealed with internal/secrets; public_key is the authorized_keys
-- line given to the user; host_key is the pinned host key (authorized_keys
-- format, '' until the user confirmed its fingerprint). jump_json lists
-- the jump hosts ([{host, port, username, host_key}]).
CREATE TABLE remote_server (
  id TEXT PRIMARY KEY,                          -- the slug [a-z0-9-]{1,32}
  name TEXT NOT NULL,
  host TEXT NOT NULL,
  port INTEGER NOT NULL DEFAULT 22 CHECK (port BETWEEN 1 AND 65535),
  username TEXT NOT NULL,
  jump_json TEXT NOT NULL DEFAULT '[]',
  host_key TEXT NOT NULL DEFAULT '',
  key_ref TEXT NOT NULL,
  public_key TEXT NOT NULL,
  platform TEXT NOT NULL DEFAULT '',            -- linux-x64 | darwin-arm64 | windows-x64 | '' (not probed)
  engine TEXT NOT NULL DEFAULT '',              -- faster-whisper | mlx | ''
  agent_version TEXT NOT NULL DEFAULT '',
  kit_hash TEXT NOT NULL DEFAULT '',            -- the installed kit ('' = not installed)
  probe_json TEXT NOT NULL DEFAULT '{}',
  settings_json TEXT NOT NULL DEFAULT '{}',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
