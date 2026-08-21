CREATE TABLE IF NOT EXISTS Links (
    ID         TEXT    NOT NULL,                                 -- normalized version of Short (foobar)
    Short      TEXT    NOT NULL DEFAULT "",                      -- user-provided Short name (Foo-Bar)
    Long       TEXT    NOT NULL DEFAULT "",
    Created    INTEGER NOT NULL DEFAULT (strftime('%s', 'now')), -- unix seconds
    Owner      TEXT    NOT NULL DEFAULT "",
    DeletedAt  INTEGER          DEFAULT NULL,                    -- unix seconds when deleted (NULL = not deleted)
    DeletedBy  TEXT             DEFAULT NULL,                    -- user who deleted the link (NULL = not deleted)
    UNIQUE (ID, Created)                                         -- each version has a unique Created timestamp
);

CREATE TABLE IF NOT EXISTS Stats (
    ID      TEXT    NOT NULL DEFAULT "",
    Created INTEGER NOT NULL DEFAULT (strftime('%s', 'now')), -- unix seconds
    Clicks  INTEGER
);
