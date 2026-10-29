package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
)

func TestStorageDeleteMetricsCollectsOneBoundedDurableSnapshot(t *testing.T) {
	o := &Observability{registry: prometheus.NewPedanticRegistry()}
	reads := 0
	err := o.RegisterStorageDeleteMetrics(func(ctx context.Context) (storage.DeleteGuardStats, error) {
		reads++
		deadline, ok := ctx.Deadline()
		if remaining := time.Until(deadline); !ok || remaining <= 0 || remaining > 3*time.Second {
			t.Error("metrics read did not receive the bounded scrape context")
		}
		return storage.DeleteGuardStats{Incomplete: 4, OldestAgeSeconds: 17.5, ReferenceConflicts: 3, UnknownWrites: 2, FailureReasons: map[string]int64{"PVC_DELETE_RESULT_UNKNOWN": 2, "": 1, "external-user-data": 1}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	families, err := o.Gatherer().Gather()
	if err != nil || reads != 1 {
		t.Fatal("scrape did not use exactly one successful read", reads, err)
	}
	want := map[string]struct {
		kind  dto.MetricType
		value float64
	}{
		"ani_storage_pvc_delete_guard_available":           {dto.MetricType_GAUGE, 1},
		"ani_storage_pvc_delete_incomplete":                {dto.MetricType_GAUGE, 4},
		"ani_storage_pvc_delete_oldest_age_seconds":        {dto.MetricType_GAUGE, 17.5},
		"ani_storage_pvc_delete_reference_conflicts_total": {dto.MetricType_COUNTER, 3},
		"ani_storage_pvc_delete_unknown_writes_total":      {dto.MetricType_COUNTER, 2},
		"ani_storage_pvc_delete_recovery_pending":          {dto.MetricType_GAUGE, 2},
	}
	if len(families) != len(want) {
		t.Fatal("unexpected metric families", len(families))
	}
	for _, family := range families {
		expected, ok := want[family.GetName()]
		if !ok || family.GetType() != expected.kind || len(family.Metric) != 1 {
			t.Fatal("unexpected metric type or cardinality", family)
		}
		metric := family.Metric[0]
		value := metric.GetGauge().GetValue()
		if expected.kind == dto.MetricType_COUNTER {
			value = metric.GetCounter().GetValue()
		}
		if value != expected.value {
			t.Fatal("metric differs from durable snapshot", family.GetName(), value, expected.value)
		}
		if family.GetName() == "ani_storage_pvc_delete_recovery_pending" && (len(metric.Label) != 1 || metric.Label[0].GetName() != "reason" || metric.Label[0].GetValue() != "PVC_DELETE_RESULT_UNKNOWN") {
			t.Fatal("recovery label is not a fixed internal reason", metric.Label)
		}
	}
}

func TestStorageDeleteMetricsUnavailableStillScrapesWithoutFalseZeros(t *testing.T) {
	o := &Observability{registry: prometheus.NewPedanticRegistry()}
	reads := 0
	err := o.RegisterStorageDeleteMetrics(func(context.Context) (storage.DeleteGuardStats, error) {
		reads++
		return storage.DeleteGuardStats{Incomplete: 99}, errors.New("durable statistics unavailable")
	})
	if err != nil {
		t.Fatal(err)
	}
	families, err := o.Gatherer().Gather()
	if err != nil || reads != 1 || len(families) != 1 || families[0].GetName() != "ani_storage_pvc_delete_guard_available" || len(families[0].Metric) != 1 || families[0].Metric[0].GetGauge().GetValue() != 0 {
		t.Fatal("failed read prevented scrape or fabricated ticket statistics", families, reads, err)
	}
}
