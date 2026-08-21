-- A service is supervised for as long as it is meant to run, so the console has
-- to answer two questions the task-shaped schema never had to: how often has
-- this execution come back, and which executions are services at all.

-- restarts is a lifetime counter, distinct from attempt: retry.reset_after
-- zeroes attempt after a healthy run, which is the point of it, and would
-- otherwise erase the only evidence that a service has been restarting.
ALTER TABLE processes ADD COLUMN restarts INTEGER NOT NULL DEFAULT 0;

-- Filtering the listing by type must stay an index search: the console polls it.
CREATE INDEX idx_processes_type_state ON processes (type, state);
