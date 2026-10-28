ALTER TABLE sandbox.sandboxes
 ADD COLUMN create_intent_digest text NOT NULL DEFAULT '',
 ADD COLUMN pending_mutation jsonb;
