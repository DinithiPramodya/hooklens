-- +goose Up

-- An endpoint is an inbox: one public URL that captures requests.
--
-- This migration deliberately creates only the columns whose concepts are
-- already covered. Authentication (owner_token), the configurable response, and
-- retention arrive in Phase 1 as migration 00002 -- by ADDING to this table, not
-- by editing this file. Once a migration has run anywhere, its ID is recorded
-- and it will never run again, so editing it changes nothing on any database
-- that already applied it. Append-only is not a style preference; it is the only
-- thing that works.
create table endpoints (
    -- uuidv7 is new in Postgres 18 and time-sortable, which matters more for
    -- the requests table than it does here. Why v7 rather than v4 is Phase 1
    -- material; for now it is an identifier.
    id          uuid        primary key default uuidv7(),

    -- The inbox name that appears in the URL, as either a7f3.hooklens.dev or
    -- hooklens.dev/e/a7f3. The application validates the format before it ever
    -- reaches SQL (see server.validSlug); the constraints here are a second
    -- line, not the only one -- the database should not depend on the
    -- application being correct.
    slug        text        not null unique
                            check (slug ~ '^[a-z0-9][a-z0-9-]{1,30}[a-z0-9]$'),

    -- A human label shown in the UI. Nullable: an inbox is usable unnamed.
    name        text,

    -- timestamptz, never timestamp. timestamp discards the offset and stores
    -- whatever wall-clock number it was handed, so two servers in two regions
    -- disagree about when a row was written. timestamptz normalises to UTC.
    created_at  timestamptz not null default now(),
    last_seen_at timestamptz
);

-- +goose Down
drop table endpoints;
