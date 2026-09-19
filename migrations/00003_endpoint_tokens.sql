-- +goose Up

-- Adding a NOT NULL column to a table that already has rows is the classic
-- migration hazard, so this is the three-step pattern rather than one
-- statement.
--
-- Step 1: add it nullable. On Postgres 11+ this is metadata-only and instant
-- even on a large table -- the rewrite that older versions performed is gone.
alter table endpoints add column owner_token_hash bytea;

-- Step 2: backfill. Existing inboxes were created before tokens existed, so
-- there is no correct value -- they were never issued one. They get the hash of
-- a random value, which is deliberately unmatchable: the inbox keeps receiving
-- but can never be read again. That is the honest outcome. Inventing a token
-- nobody was given would be worse, and leaving them readable-by-anyone worse
-- still.
--
-- sha256() and random() are both core Postgres; gen_random_bytes() would need
-- the pgcrypto extension for no gain here.
update endpoints
   set owner_token_hash = sha256((random()::text || clock_timestamp()::text)::bytea)
 where owner_token_hash is null;

-- Step 3: enforce. On a populated table this takes an ACCESS EXCLUSIVE lock and
-- scans every row to prove no nulls remain -- brief here, minutes on a large
-- table, during which all reads and writes block. The production-safe variant
-- is to add a CHECK (... IS NOT NULL) NOT VALID, VALIDATE it separately under a
-- weaker lock, then attach it. Noted rather than done: this table has single
-- digits of rows.
alter table endpoints alter column owner_token_hash set not null;

-- Look-ups authenticate by slug first and then compare the hash, so no index on
-- the hash is needed -- and adding one would be actively unhelpful, since an
-- index on a secret makes it enumerable through timing.

-- +goose Down
alter table endpoints drop column owner_token_hash;
