-- Manual public egress evidence is diagnostic metadata, not an allocation gate.
-- Preserve the independent Intranet evidence constraint and all existing rows.
DO $$
BEGIN
 IF NOT EXISTS (
  SELECT 1 FROM pg_constraint
  WHERE conrelid='network_public_pools'::regclass
    AND conname='network_public_pools_check3' AND contype='c'
    AND pg_get_constraintdef(oid)='CHECK (((NOT allocation_enabled) OR (verification IS NOT NULL)))'
 ) THEN
  RAISE EXCEPTION 'unexpected address pool allocation verification constraint';
 END IF;
END $$;
ALTER TABLE network_public_pools DROP CONSTRAINT network_public_pools_check3;
ALTER TABLE network_public_pools ADD CONSTRAINT network_pool_allocation_verification
 CHECK (scope='public' OR NOT allocation_enabled OR verification IS NOT NULL);
