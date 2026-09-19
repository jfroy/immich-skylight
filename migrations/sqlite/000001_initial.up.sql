-- Key/value store for installation metadata and Skylight OAuth tokens.
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- One row per Immich asset that has been uploaded to at least one frame.
CREATE TABLE sent (
    asset_id TEXT PRIMARY KEY,
    checksum TEXT,
    sent_at  TEXT NOT NULL -- RFC 3339 UTC
) STRICT;

-- Skylight message IDs created for an asset, per frame. Used for reverse sync.
CREATE TABLE sent_message (
    asset_id   TEXT    NOT NULL REFERENCES sent (asset_id) ON DELETE CASCADE,
    frame_id   TEXT    NOT NULL,
    message_id INTEGER NOT NULL,
    PRIMARY KEY (asset_id, frame_id, message_id)
) STRICT;

CREATE INDEX sent_message_frame_id ON sent_message (frame_id);
