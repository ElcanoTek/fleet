-- Team sync for the account-events feed (docs/ACCOUNT-EVENTS.md).
--
-- account_events.team is the account's users.team_id at the time of the event
-- ('' = no team), so a team-only change is a real change the feed reports.
-- Rows queued before this migration report '' until the account's next event.
ALTER TABLE account_events ADD COLUMN team TEXT NOT NULL DEFAULT '';

-- external_access_state.team is the team the identity provider last sent for
-- the account. NULL means the provider does not manage the account's team
-- (an older Auth, a migration backfill, or team sync switched off), and Fleet
-- then leaves users.team_id alone.
ALTER TABLE external_access_state ADD COLUMN team TEXT;
