-- Timestamps are stored as text and compared as text, so the format has to sort
-- the way the instants do. RFC3339Nano does not: it strips trailing zeros from
-- the fraction, and a value with no fraction at all ends in 'Z', which is
-- greater than the '.' that starts one.
--
-- The visible consequence is the range filters of GET /v1/processes. A client
-- sends whole seconds -- created_after=2026-08-22T10:00:00Z -- and every
-- execution created during that second sorts *below* the bound, so the filter
-- silently drops exactly the rows it was asked for.
--
-- From here on the fraction is always nine digits. This pass rewrites what is
-- already stored, so the two forms never have to be compared with each other.

-- Padding is the same expression everywhere: keep everything up to the dot,
-- then the fraction right-padded to nine digits, then the zone. rtrim removes
-- the trailing 'Z'; the daemon only ever writes UTC.

UPDATE processes SET
    created_at = CASE
        WHEN instr(created_at, '.') = 0 THEN rtrim(created_at, 'Z') || '.000000000Z'
        ELSE substr(created_at, 1, instr(created_at, '.'))
             || substr(rtrim(substr(created_at, instr(created_at, '.') + 1), 'Z') || '000000000', 1, 9)
             || 'Z'
    END,
    queued_at = CASE
        WHEN queued_at IS NULL OR queued_at = '' THEN queued_at
        WHEN instr(queued_at, '.') = 0 THEN rtrim(queued_at, 'Z') || '.000000000Z'
        ELSE substr(queued_at, 1, instr(queued_at, '.'))
             || substr(rtrim(substr(queued_at, instr(queued_at, '.') + 1), 'Z') || '000000000', 1, 9)
             || 'Z'
    END,
    started_at = CASE
        WHEN started_at IS NULL OR started_at = '' THEN started_at
        WHEN instr(started_at, '.') = 0 THEN rtrim(started_at, 'Z') || '.000000000Z'
        ELSE substr(started_at, 1, instr(started_at, '.'))
             || substr(rtrim(substr(started_at, instr(started_at, '.') + 1), 'Z') || '000000000', 1, 9)
             || 'Z'
    END,
    finished_at = CASE
        WHEN finished_at IS NULL OR finished_at = '' THEN finished_at
        WHEN instr(finished_at, '.') = 0 THEN rtrim(finished_at, 'Z') || '.000000000Z'
        ELSE substr(finished_at, 1, instr(finished_at, '.'))
             || substr(rtrim(substr(finished_at, instr(finished_at, '.') + 1), 'Z') || '000000000', 1, 9)
             || 'Z'
    END,
    retry_at = CASE
        WHEN retry_at IS NULL OR retry_at = '' THEN retry_at
        WHEN instr(retry_at, '.') = 0 THEN rtrim(retry_at, 'Z') || '.000000000Z'
        ELSE substr(retry_at, 1, instr(retry_at, '.'))
             || substr(rtrim(substr(retry_at, instr(retry_at, '.') + 1), 'Z') || '000000000', 1, 9)
             || 'Z'
    END;

UPDATE attempts SET
    started_at = CASE
        WHEN started_at IS NULL OR started_at = '' THEN started_at
        WHEN instr(started_at, '.') = 0 THEN rtrim(started_at, 'Z') || '.000000000Z'
        ELSE substr(started_at, 1, instr(started_at, '.'))
             || substr(rtrim(substr(started_at, instr(started_at, '.') + 1), 'Z') || '000000000', 1, 9)
             || 'Z'
    END,
    finished_at = CASE
        WHEN finished_at IS NULL OR finished_at = '' THEN finished_at
        WHEN instr(finished_at, '.') = 0 THEN rtrim(finished_at, 'Z') || '.000000000Z'
        ELSE substr(finished_at, 1, instr(finished_at, '.'))
             || substr(rtrim(substr(finished_at, instr(finished_at, '.') + 1), 'Z') || '000000000', 1, 9)
             || 'Z'
    END;

UPDATE locks SET
    acquired_at = CASE
        WHEN instr(acquired_at, '.') = 0 THEN rtrim(acquired_at, 'Z') || '.000000000Z'
        ELSE substr(acquired_at, 1, instr(acquired_at, '.'))
             || substr(rtrim(substr(acquired_at, instr(acquired_at, '.') + 1), 'Z') || '000000000', 1, 9)
             || 'Z'
    END;

UPDATE idempotency_keys SET
    created_at = CASE
        WHEN instr(created_at, '.') = 0 THEN rtrim(created_at, 'Z') || '.000000000Z'
        ELSE substr(created_at, 1, instr(created_at, '.'))
             || substr(rtrim(substr(created_at, instr(created_at, '.') + 1), 'Z') || '000000000', 1, 9)
             || 'Z'
    END;

UPDATE audit_log SET
    at = CASE
        WHEN instr(at, '.') = 0 THEN rtrim(at, 'Z') || '.000000000Z'
        ELSE substr(at, 1, instr(at, '.'))
             || substr(rtrim(substr(at, instr(at, '.') + 1), 'Z') || '000000000', 1, 9)
             || 'Z'
    END;

UPDATE schedules SET
    last_fired_at = CASE
        WHEN last_fired_at IS NULL OR last_fired_at = '' THEN last_fired_at
        WHEN instr(last_fired_at, '.') = 0 THEN rtrim(last_fired_at, 'Z') || '.000000000Z'
        ELSE substr(last_fired_at, 1, instr(last_fired_at, '.'))
             || substr(rtrim(substr(last_fired_at, instr(last_fired_at, '.') + 1), 'Z') || '000000000', 1, 9)
             || 'Z'
    END,
    last_missed_at = CASE
        WHEN last_missed_at IS NULL OR last_missed_at = '' THEN last_missed_at
        WHEN instr(last_missed_at, '.') = 0 THEN rtrim(last_missed_at, 'Z') || '.000000000Z'
        ELSE substr(last_missed_at, 1, instr(last_missed_at, '.'))
             || substr(rtrim(substr(last_missed_at, instr(last_missed_at, '.') + 1), 'Z') || '000000000', 1, 9)
             || 'Z'
    END;
