-- +goose Up

-- Add 'overloaded' to the forwarding outcome codes.
--
-- A tunnel has a bounded number of in-flight requests (failure mode 9), and a
-- capture that never gets a slot before its deadline has to be recorded as
-- something. Folding it into 'timeout' was the tempting shortcut and is wrong
-- for the reason every other distinction in this schema exists: the two send a
-- developer to different places. 'timeout' says their handler is slow.
-- 'overloaded' says we never asked it.
--
-- A CHECK constraint cannot be altered in place, so this drops and recreates
-- it. Both statements are in one migration and therefore one transaction --
-- see docs/learn/03-migrations.md -- so there is no window in which the column
-- is unconstrained.
alter table requests drop constraint requests_forward_error_check;

alter table requests add constraint requests_forward_error_check
    check (forward_error is null or forward_error in
           ('no_tunnel', 'timeout', 'disconnected', 'unreachable', 'protocol',
            'overloaded'));

-- +goose Down

-- Existing rows must satisfy the narrower constraint before it can be applied,
-- so any 'overloaded' rows are reset to NULL rather than blocking the
-- rollback. That loses information, which is the honest cost of going
-- backwards: the alternative is a down migration that fails on a busy
-- database, which is worse than a lossy one.
update requests set forward_error = null where forward_error = 'overloaded';

alter table requests drop constraint requests_forward_error_check;

alter table requests add constraint requests_forward_error_check
    check (forward_error is null or forward_error in
           ('no_tunnel', 'timeout', 'disconnected', 'unreachable', 'protocol'));
