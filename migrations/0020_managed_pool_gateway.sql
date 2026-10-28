-- Existing pools retain their externally selected gateways. Only new requests
-- omitting gateway_id own a gateway and are responsible for its retirement.
ALTER TABLE network_public_pools ADD COLUMN managed_gateway boolean NOT NULL DEFAULT false;
ALTER TABLE network_public_pools ADD CONSTRAINT network_managed_gateway_public
 CHECK (NOT managed_gateway OR (scope='public' AND gateway_id IS NOT NULL));
CREATE UNIQUE INDEX network_owned_pool_gateway ON network_public_pools(cluster_id,gateway_id)
 WHERE managed_gateway;
