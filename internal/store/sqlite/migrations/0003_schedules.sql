-- A schedule fires whether or not anyone is watching, so what it did has to
-- outlive the process that did it. Without a persisted last firing, a daemon
-- restart makes an occurrence either vanish or repeat, and neither is visible.

CREATE TABLE schedules (
    worker         TEXT PRIMARY KEY,

    -- The occurrence the schedule last fired for, in UTC. It is the scheduled
    -- instant, not the moment the execution started: jitter and dispatch delay
    -- must not shift the grid the next occurrence is computed from.
    last_fired_at  TEXT,

    -- The most recent occurrence that passed with the daemon down, and how many
    -- have done so in total. A missed run that is not caught up is still a fact
    -- an operator has to be able to read.
    last_missed_at TEXT,
    missed_runs    INTEGER NOT NULL DEFAULT 0
);
