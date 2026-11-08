SET LOCAL lock_timeout = '5s';
-- Widen notification kinds for assignment changes and refused renegotiations.
ALTER TABLE user_notifications DROP CONSTRAINT user_notifications_kind_check;
ALTER TABLE user_notifications ADD CONSTRAINT user_notifications_kind_check
    CHECK (kind IN ('assigned', 'comment', 'reply', 'reminder', 'mention', 'unassigned', 'renegotiation_refused'));
