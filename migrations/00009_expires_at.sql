-- +goose Up

-- Give every request its own expiry, so the retention sweep can use an index.
--
-- The sweep deletes requests past their inbox's retention window. The window
-- lived on `endpoints` and the timestamp on `requests`, so the predicate
-- spanned two tables:
--
--   where r.received_at < now() - make_interval(hours => e.retention_hours)
--
-- Postgres cannot evaluate that until it has joined, so it read every row and
-- filtered afterwards -- and LIMIT does not help a filter that runs after the
-- join. Measured at 576,639 rows: a 276ms sequential scan, every tick, to
-- return ZERO rows. The steady state is the expensive one, and it grows
-- linearly with the table the sweep exists to keep small.
--
-- Storing the expiry on the row is deliberate denormalisation. The value is
-- derived and now has a second copy, which can go stale -- see the Down
-- section and store.SetRetention for who maintains it.
-- docs/learn/37-indexable-predicates.md

alter table requests add column expires_at timestamptz;

-- Backfill from the current retention setting. One pass, at migration time,
-- rather than a nullable column the sweep has to special-case forever.
update requests r
   set expires_at = r.received_at + make_interval(hours => e.retention_hours)
  from endpoints e
 where e.id = r.endpoint_id;

-- NOT NULL only after the backfill, and it is worth having: a NULL here would
-- mean "never expires", silently, and a row that quietly opts out of retention
-- is exactly the bug this column exists to prevent.
alter table requests alter column expires_at set not null;

-- A plain btree, not partial. A partial index needs a constant predicate and
-- ours is `expires_at < now()`, which moves; and since almost every row is in
-- the future, the index's value is that the scan STOPS -- it walks in order
-- from the oldest and the LIMIT ends it after N.
--
-- Built inside the migration's transaction, which takes a lock on the table.
-- Not CONCURRENTLY: that cannot run in a transaction, so it would mean
-- opting this file out of one with goose's no-transaction annotation, and
-- giving up the all-or-nothing property that makes a failed migration safe
-- (see docs/learn/03-migrations.md). (That annotation is deliberately not
-- spelled out here: goose reads ANY comment line containing its marker as a
-- directive, so writing it in prose is a parse error -- found by doing it.)
-- For a
-- single-instance self-hosted tool a sub-second lock is the better trade. The
-- signal to change it is a deployment where this table is large enough that
-- the lock is an outage.
create index requests_expires_at_idx on requests (expires_at);

-- +goose Down

drop index requests_expires_at_idx;

alter table requests drop column expires_at;
