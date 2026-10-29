package instance

import (
	"context"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
)

func (p *Postgres) ObservePVCDeleteWrite(outcome string) {
	if outcome == "unknown" {
		p.pvcUnknownWrites.Add(1)
	}
}

func (p *Postgres) PVCDeleteStats(ctx context.Context) (storage.DeleteGuardStats, error) {
	s := storage.DeleteGuardStats{ReferenceConflicts: p.pvcReferenceConflicts.Load(), UnknownWrites: p.pvcUnknownWrites.Load(), FailureReasons: map[string]int64{}}
	rows, err := p.pool.Query(ctx, `SELECT last_reason,count(*),COALESCE(EXTRACT(EPOCH FROM clock_timestamp()-min(created_at)),0)::double precision FROM instance.borrowed_pvc_guards WHERE phase IN ('reserved','deleting','observing') GROUP BY last_reason`)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var reason string
		var count int64
		var age float64
		if err := rows.Scan(&reason, &count, &age); err != nil {
			return s, err
		}
		s.Incomplete += count
		if age > s.OldestAgeSeconds {
			s.OldestAgeSeconds = age
		}
		if reason != "" {
			s.FailureReasons[reason] = count
		}
	}
	return s, rows.Err()
}
