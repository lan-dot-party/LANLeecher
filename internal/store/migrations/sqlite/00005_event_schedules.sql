-- +goose Up
-- Planned events: LANLeecher starts the event at starts_at and stops it at
-- ends_at (exclusive), so nobody has to remember to click "Start event".
-- event_id links the event once it has been started (by the schedule or
-- early by hand); a schedule whose window passed without a start is "missed".
CREATE TABLE event_schedules (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    notes      TEXT    NOT NULL DEFAULT '',
    starts_at  INTEGER NOT NULL,
    ends_at    INTEGER NOT NULL,
    event_id   INTEGER REFERENCES events(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL,
    CHECK (ends_at > starts_at)
);
CREATE INDEX event_schedules_starts ON event_schedules (starts_at);

-- +goose Down
DROP TABLE event_schedules;
