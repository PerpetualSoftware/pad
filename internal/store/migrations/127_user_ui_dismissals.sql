-- Migration 127: which one-time UI suggestions a user has dismissed (TASK-3452).
--
-- A JSON array of keys, each from models.UIDismissalKeys (an allow-list the
-- store and the handler both enforce, so this never becomes free-form
-- storage). Today: the two first-time tutorial cards on Pad Cloud. On the
-- user, not in the browser, because a dismissal is a promise to the person:
-- a card dismissed on one device must not come back on the next. A column
-- rather than a table by ruling (lead, day 89): generalise only if a third
-- kind of dismissal appears.
ALTER TABLE users ADD COLUMN ui_dismissals TEXT NOT NULL DEFAULT '[]';
