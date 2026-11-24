-- 0212_server_presence: which fleet servers run against this database.
--
-- Every `fleet serve` writes its row here every 15 seconds while it runs
-- (store.HoldServerPresence) and removes it when it stops. An account
-- deletion from the CLI (`fleet admin rm`, `fleet chat user del`) refuses
-- while any row was written in the last 45 seconds (store.ExcludeServers),
-- in addition to the advisory lock the servers hold: the row covers the
-- moments a server's lock connection has dropped and is being replaced. A
-- row a crashed server left stops blocking after those 45 seconds, and a
-- running server removes rows older than an hour.
--
-- last_seen is the database's clock (now()), so servers and the CLI on other
-- hosts compare the same clock.
CREATE TABLE IF NOT EXISTS core.server_presence (
    instance_id TEXT        PRIMARY KEY,
    last_seen   TIMESTAMPTZ NOT NULL DEFAULT now()
);
