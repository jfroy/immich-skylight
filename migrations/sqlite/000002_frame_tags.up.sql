-- Immich tag created for each Skylight frame (IMMICH_FRAME_TAG_TEMPLATE).
-- Lets the daemon recognize its own tag across frame renames and prune it
-- when a frame is no longer a target.
CREATE TABLE frame_tag (
    frame_id   TEXT PRIMARY KEY,
    tag_id     TEXT NOT NULL,
    tag_value  TEXT NOT NULL, -- full path as last seen, e.g. "Skylight/Kitchen"
    updated_at TEXT NOT NULL  -- RFC 3339 UTC
) STRICT;
