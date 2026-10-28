-- A platform default outlet owns an EIP independently of tenant inventory.
ALTER TABLE network_default_vpc_egresses ADD COLUMN eip_id text;
UPDATE network_default_vpc_egresses SET eip_id='eip_' || substring(resource_id FROM 5);
ALTER TABLE network_default_vpc_egresses
  ALTER COLUMN eip_id SET NOT NULL,
  ADD CONSTRAINT network_default_egress_eip_identity CHECK (eip_id ~ '^eip_[0-9a-f]{32}$'),
  ADD CONSTRAINT network_default_egress_eip_unique UNIQUE (eip_id);
