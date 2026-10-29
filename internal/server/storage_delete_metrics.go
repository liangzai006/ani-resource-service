package server

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
)

type storageDeleteCollector struct {
	read                                                       func(context.Context) (storage.DeleteGuardStats, error)
	available, incomplete, oldest, conflicts, unknown, reasons *prometheus.Desc
}

func (c *storageDeleteCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, description := range []*prometheus.Desc{c.available, c.incomplete, c.oldest, c.conflicts, c.unknown, c.reasons} {
		ch <- description
	}
}

func (c *storageDeleteCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stats, err := c.read(ctx)
	if err != nil || ctx.Err() != nil {
		// Missing evidence remains absent. A failed read must not claim that
		// outstanding tickets, unknown writes or conflicts have become zero.
		ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.available, prometheus.GaugeValue, 1)
	ch <- prometheus.MustNewConstMetric(c.incomplete, prometheus.GaugeValue, float64(stats.Incomplete))
	ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, stats.OldestAgeSeconds)
	ch <- prometheus.MustNewConstMetric(c.conflicts, prometheus.CounterValue, float64(stats.ReferenceConflicts))
	ch <- prometheus.MustNewConstMetric(c.unknown, prometheus.CounterValue, float64(stats.UnknownWrites))
	for reason, count := range stats.FailureReasons {
		if count <= 0 || !storageDeleteMetricReason(reason) {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.reasons, prometheus.GaugeValue, float64(count), reason)
	}
}

// Only internal recovery reasons become labels; object names and external
// driver errors must never create metric label values.
func storageDeleteMetricReason(reason string) bool {
	switch reason {
	case "NAMESPACE_UNAVAILABLE", "PVC_OBSERVATION_UNAVAILABLE", "PVC_OWNERSHIP_CHANGED", "VOLUME_USAGE_UNKNOWN", "VOLUME_IN_USE", "PVC_DELETE_RESULT_UNKNOWN", "PVC_DELETE_REJECTED":
		return true
	default:
		return false
	}
}

func (o *Observability) RegisterStorageDeleteMetrics(read func(context.Context) (storage.DeleteGuardStats, error)) error {
	if o == nil || o.registry == nil || read == nil {
		return fmt.Errorf("Storage delete metrics require a registry and reader")
	}
	collector := &storageDeleteCollector{
		read:       read,
		available:  prometheus.NewDesc("ani_storage_pvc_delete_guard_available", "Whether this scrape read the durable PVC delete guard statistics successfully.", nil, nil),
		incomplete: prometheus.NewDesc("ani_storage_pvc_delete_incomplete", "Number of incomplete durable PVC delete tickets.", nil, nil),
		oldest:     prometheus.NewDesc("ani_storage_pvc_delete_oldest_age_seconds", "Age in seconds of the oldest incomplete PVC delete ticket.", nil, nil),
		conflicts:  prometheus.NewDesc("ani_storage_pvc_delete_reference_conflicts_total", "PVC delete reservations rejected by protected borrowed references.", nil, nil),
		unknown:    prometheus.NewDesc("ani_storage_pvc_delete_unknown_writes_total", "PVC delete writes whose external result was unknown.", nil, nil),
		reasons:    prometheus.NewDesc("ani_storage_pvc_delete_recovery_pending", "Incomplete PVC delete tickets grouped by fixed internal recovery reason.", []string{"reason"}, nil),
	}
	return o.registry.Register(collector)
}
