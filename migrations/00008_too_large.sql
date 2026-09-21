-- +goose Up

-- Add 'too_large' to the forwarding outcome codes.
--
-- A capture whose body hit the ingest limit is stored with body_truncated
-- set, and is deliberately NOT forwarded: handing a local app bytes that are
-- corrupt but look complete is worse than handing it nothing, because the
-- resulting parse or signature failure blames the sender rather than the
-- truncation. See docs/learn/24-size-limits.md.
--
-- Distinct from every existing code for the usual reason: it is the only one
-- the user can fix themselves, by raising HOOKLENS_MAX_BODY.
alter table requests drop constraint requests_forward_error_check;

alter table requests add constraint requests_forward_error_check
    check (forward_error is null or forward_error in
           ('no_tunnel', 'timeout', 'disconnected', 'unreachable', 'protocol',
            'overloaded', 'too_large'));

-- +goose Down

update requests set forward_error = null where forward_error = 'too_large';

alter table requests drop constraint requests_forward_error_check;

alter table requests add constraint requests_forward_error_check
    check (forward_error is null or forward_error in
           ('no_tunnel', 'timeout', 'disconnected', 'unreachable', 'protocol',
            'overloaded'));
