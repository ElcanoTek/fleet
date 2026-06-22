-- Per-conversation runtime flavor (fleet's ACP runtime selection). Empty =
-- the bundle's default flavor (native-inprocess). Set via the chat flavor
-- picker (PUT /conversations/{id}/runtime). The recognized values are the
-- bundle manifest's runtimes: keys (native-inprocess, native-acp, ...); an
-- unknown value falls back to the default at run time, so a stale picker
-- value can never wedge a turn.

ALTER TABLE conversations
    ADD COLUMN runtime TEXT NOT NULL DEFAULT '';
