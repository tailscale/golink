-- +goose Up
-- Add an optional description for each link (tailscale/golink#70). The column is
-- nullable: links without a description store NULL rather than an empty string.
ALTER TABLE Links ADD COLUMN Description TEXT;

-- +goose Down
ALTER TABLE Links DROP COLUMN Description;
