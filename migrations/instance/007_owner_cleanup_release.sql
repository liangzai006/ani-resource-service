-- Existing pending events are not eligible until the owner re-observes and
-- commits its complete cleanup fact. Preserve every original event and ACK.
ALTER TABLE instance.instance_quota_release_outbox
 ADD COLUMN cleanup_completed_at timestamptz,
 ADD COLUMN cleanup_version bigint CHECK(cleanup_version>0),
 ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 ADD CONSTRAINT owner_cleanup_release_fact CHECK
  ((cleanup_completed_at IS NULL) = (cleanup_version IS NULL));
CREATE INDEX instance_quota_release_due
 ON instance.instance_quota_release_outbox(next_attempt_at,event_id)
 WHERE NOT acknowledged AND cleanup_completed_at IS NOT NULL;
