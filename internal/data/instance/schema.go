package instance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	migrations "github.com/zhangzhe-ctrl/ani-resource-service/migrations/instance"
)

// Instance retains every historical checksum, including its original catalog DDL.
// It does not require the catalog relation to remain in the instance schema.
var instanceSchemaFiles = []string{"002_compute_presets.sql", "003_whole_gpu_quantity.sql", "004_gpu_execution.sql", "005_instance_configuration.sql", "006_instance_network_submissions.sql", "007_owner_cleanup_release.sql", "008_restart_pvc.sql"}

func (p *Postgres) checkInstanceSchema(ctx context.Context) error {
	for i, name := range instanceSchemaFiles {
		body, err := migrations.Files.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		var checksum string
		if err = p.pool.QueryRow(ctx, "SELECT checksum FROM instance.schema_version WHERE version=$1", i+2).Scan(&checksum); err != nil || checksum != hex.EncodeToString(sum[:]) {
			return fmt.Errorf("instance schema %s not migrated or checksum differs", name)
		}
	}
	return nil
}
