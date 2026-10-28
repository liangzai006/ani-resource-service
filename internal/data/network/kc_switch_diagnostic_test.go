package data

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestFrozenSwitchFailuresRetainSpecificCause(t *testing.T) {
	for _, name := range []string{"node not ready", "node facts expired", "node facts missing", "physical interface unsafe"} {
		t.Run(name, func(t *testing.T) {
			sw, nodes, maps, daemons, _, now := switchFixture(t)
			sw.Nodes = []biz.VirtualSwitchNode{{NodeName: nodes[0].GetName(), NodeUID: string(nodes[0].GetUID()), MAC: "02:00:00:00:00:01"}}
			var doc NodeFactsDocument
			if err := json.Unmarshal([]byte(crString(&maps[0], "data", "facts.json")), &doc); err != nil {
				t.Fatal(err)
			}
			sw.Nodes[0].MAC = doc.Interfaces[0].MAC
			kind := biz.ProviderTemporary
			switch name {
			case "node not ready":
				_ = unstructured.SetNestedSlice(nodes[0].Object, []any{map[string]any{"type": "Ready", "status": "False"}}, "status", "conditions")
			case "node facts expired":
				doc.CollectedAt = now.Add(-2 * time.Minute)
			case "node facts missing":
				maps = nil
			case "physical interface unsafe":
				doc.Interfaces[0].Carrier = false
				kind = biz.ProviderConflict
			}
			if maps != nil {
				body, _ := json.Marshal(doc)
				_ = unstructured.SetNestedField(maps[0].Object, string(body), "data", "facts.json")
			}
			_, err := inspectFrozenSwitchPhysical(sw, nodes, maps, daemons, now)
			var failure *biz.ProviderError
			if !errors.As(err, &failure) || failure.Kind != kind || failure.Cause == nil || !strings.Contains(failure.Cause.Error(), name) || !strings.Contains(failure.Cause.Error(), nodes[0].GetName()) {
				t.Fatalf("missing specific branch and node diagnostic: %#v", err)
			}
		})
	}
}

func TestFrozenSwitchObservationRetainsPhysicalEvidenceTime(t *testing.T) {
	sw, nodes, maps, daemons, _, now := switchFixture(t)
	var doc NodeFactsDocument
	if err := json.Unmarshal([]byte(crString(&maps[0], "data", "facts.json")), &doc); err != nil {
		t.Fatal(err)
	}
	doc.CollectedAt = now.Add(-20 * time.Second)
	sw.Nodes = []biz.VirtualSwitchNode{{NodeName: nodes[0].GetName(), NodeUID: string(nodes[0].GetUID()), MAC: doc.Interfaces[0].MAC}}
	body, _ := json.Marshal(doc)
	_ = unstructured.SetNestedField(maps[0].Object, string(body), "data", "facts.json")
	at, err := inspectFrozenSwitchPhysical(sw, nodes, maps, daemons, now)
	if err != nil || !at.Equal(doc.CollectedAt) {
		t.Fatalf("observation time=%s want actual collected_at=%s, err=%v", at, doc.CollectedAt, err)
	}
}
