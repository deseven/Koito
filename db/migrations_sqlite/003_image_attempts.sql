-- +goose Up

-- Track when an image fetch was last attempted for a release/artist, so that
-- entities without an image are not re-attempted on every startup. A value of 0
-- means "never attempted". Views select `r.*` / `a.*`, so the added columns are
-- visible to them without being recreated.
ALTER TABLE releases ADD COLUMN image_attempted_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE artists ADD COLUMN image_attempted_at INTEGER NOT NULL DEFAULT 0;

-- +goose Down

ALTER TABLE releases DROP COLUMN image_attempted_at;
ALTER TABLE artists DROP COLUMN image_attempted_at;
