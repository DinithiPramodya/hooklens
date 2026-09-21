-- +goose Up

-- What happened when we tried to hand this capture to a tunnel.
--
-- Nullable, and null means "we have not tried yet" rather than "it failed".
-- That distinction matters because the row is inserted BEFORE forwarding is
-- attempted -- the durability line in internal/ingest requires it -- so every
-- row is briefly null here, and a row that stays null is one where the process
-- died mid-request. A NOT NULL default would have erased that signal.
alter table requests
    -- The status the local app returned, when it was reached at all.
    add column forward_status integer
        check (forward_status is null or forward_status between 100 and 599),

    -- Why the local app was NOT reached. Mutually exclusive with
    -- forward_status: one of them describes what happened, never both.
    --
    -- A short machine-readable code rather than free text, because the UI
    -- switches on it and a log message written for humans would change
    -- wording and silently break that. The reason a person reads lives in the
    -- code's mapping, not in the column.
    add column forward_error text
        check (forward_error is null or forward_error in
               ('no_tunnel', 'timeout', 'disconnected', 'unreachable', 'protocol')),

    -- How long the round trip took, successful or not. Null when never tried.
    -- Milliseconds as an integer rather than an interval: it is only ever read
    -- as a number and displayed as one.
    add column forward_ms integer check (forward_ms is null or forward_ms >= 0),

    -- The invariant above, enforced rather than merely documented. A row
    -- claiming both a status and an error is a bug in the handler, and this is
    -- where it gets caught instead of being rendered as a contradiction.
    add constraint forward_outcome_exclusive
        check (forward_status is null or forward_error is null);

-- Deliberately no index. Nothing filters on these columns: they are read as
-- part of a row already located by (endpoint_id, received_at desc), and the
-- same reasoning as 00005 applies -- every capture would pay to maintain an
-- index that only a future analytics query would use.

-- +goose Down
alter table requests
    drop constraint forward_outcome_exclusive,
    drop column forward_status,
    drop column forward_error,
    drop column forward_ms;
