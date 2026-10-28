package data

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/data/network/sqlcgen"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
)

type ObservationOptions struct {
	AuditInterval, AuditJitter, AuditTimeout, FlushInterval time.Duration
	QueueCapacity                                           int
}

func DefaultObservationOptions() ObservationOptions {
	return ObservationOptions{30 * time.Second, 2 * time.Second, 10 * time.Second, 100 * time.Millisecond, 4096}
}

type observationTarget struct{ tenant, id, kind string }

func (t observationTarget) key() string { return t.kind + "/" + t.tenant + "/" + t.id }

type auditView struct {
	collected       time.Time
	sequence, epoch uint64
	indices         map[schema.GroupVersionResource]cache.Indexer
	generations     map[string]int64
	targets         map[string][]observationTarget
	bytes           int64
}

// An audit result belongs to its own completion channel. A later collection
// must not overwrite the result observed by a waiter on this one.
type auditCollection struct {
	done chan struct{}
	view *auditView
	err  error
}

// KCObservation supplies invalidation hints and independently audited facts.
// Only domain workers apply facts. Watch never writes product state or freshness.
type KCObservation struct {
	provider          *KCProvider
	client            dynamic.Interface
	options           ObservationOptions
	informers         []cache.SharedIndexInformer
	mu                sync.Mutex
	view              *auditView
	pending           map[string]struct{}
	pendingSince      time.Time
	changes           map[string]uint64
	attachmentChanges map[string]uint64
	sequence, epoch   uint64
	overflow          bool
	refreshing        *auditCollection
	cancel            context.CancelFunc
	ctx               context.Context
	done              chan struct{}
	started           bool
	synced            atomic.Bool
	auditSuccess      atomic.Int64
	watchErrors       atomic.Int64
	sourceStarts      atomic.Int64
	watchIndexBytes   atomic.Int64
	auditFailures     atomic.Int64
	auditMaxSeconds   float64
	collectingSince   time.Time
	notifications     atomic.Int64
	coalesced         atomic.Int64
	overflows         atomic.Int64
	candidates        atomic.Int64
	pgMetrics         map[string]float64
}

func (p *KCProvider) EnableObservation(options ObservationOptions) (*KCObservation, error) {
	if options.AuditInterval <= 0 || options.AuditTimeout <= 0 || options.FlushInterval <= 0 || options.QueueCapacity < 16 || p.observation != nil {
		return nil, fmt.Errorf("invalid observation lifecycle/options")
	}
	o := &KCObservation{provider: p, client: p.watchClient, options: options, pending: map[string]struct{}{}, changes: map[string]uint64{}, done: make(chan struct{})}
	for _, gvr := range p.observationResources() {
		resource := o.client.Resource(gvr)
		informer := cache.NewSharedIndexInformer(&cache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
				return resource.List(ctx, opts)
			},
			WatchFuncWithContext: func(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
				o.sourceBoundary()
				return resource.Watch(ctx, opts)
			},
		}, &unstructured.Unstructured{}, 0, cache.Indexers{relationshipIndex: relationshipKeys})
		_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: func(obj any) { o.changed(nil, obj) }, UpdateFunc: o.changed, DeleteFunc: func(obj any) { o.changed(obj, nil) }})
		if err != nil {
			return nil, err
		}
		if err = informer.SetWatchErrorHandler(func(_ *cache.Reflector, err error) { o.watchErrors.Add(1); o.sourceBoundary() }); err != nil {
			return nil, err
		}
		o.informers = append(o.informers, informer)
	}
	p.observation = o
	return o, nil
}

// A new Watch request or a reported source failure is a continuity boundary.
// In-flight independent collections cannot inherit proof from before it. The
// full audit recovers the scope; ordinary unrelated object events remain keyed.
func (o *KCObservation) sourceBoundary() {
	o.mu.Lock()
	o.epoch++
	o.overflow = true
	o.mu.Unlock()
	o.sourceStarts.Add(1)
}

func unwrapObject(value any) *unstructured.Unstructured {
	if t, ok := value.(cache.DeletedFinalStateUnknown); ok {
		value = t.Obj
	}
	o, _ := value.(*unstructured.Unstructured)
	return o
}
func (o *KCObservation) changed(old, new any) {
	before, after := unwrapObject(old), unwrapObject(new)
	if before != nil && after != nil && reflect.DeepEqual(before.Object, after.Object) {
		o.coalesced.Add(1)
		return
	}
	for _, item := range []struct {
		object *unstructured.Unstructured
		sign   int64
	}{{before, -1}, {after, 1}} {
		if item.object != nil {
			body, _ := json.Marshal(item.object.Object)
			o.watchIndexBytes.Add(item.sign * int64(len(body)))
		}
	}
	if unchangedNodeFactsRenewal(before, after, time.Now()) || unchangedNodeNetworkObservation(before, after) {
		o.coalesced.Add(1)
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sequence++
	for _, object := range []*unstructured.Unstructured{before, after} {
		if object == nil {
			continue
		}
		keys, _ := relationshipKeys(object)
		for _, key := range keys {
			if len(o.changes) >= o.options.QueueCapacity*8 {
				clear(o.changes)
				clear(o.attachmentChanges)
				o.epoch++
				o.overflow = true
				o.overflows.Add(1)
			}
			o.changes[key] = o.sequence
			if attachmentObservationObject(object) {
				if o.attachmentChanges == nil {
					o.attachmentChanges = map[string]uint64{}
				}
				o.attachmentChanges[key] = o.sequence
			}
			if _, ok := o.pending[key]; ok {
				o.coalesced.Add(1)
				continue
			}
			if len(o.pending) >= o.options.QueueCapacity {
				o.overflow = true
				o.overflows.Add(1)
				continue
			}
			if len(o.pending) == 0 {
				o.pendingSince = time.Now()
			}
			o.pending[key] = struct{}{}
		}
	}
}
func (o *KCObservation) Start(parent context.Context) error {
	o.mu.Lock()
	if o.started {
		o.mu.Unlock()
		return fmt.Errorf("observer already started")
	}
	o.started = true
	o.ctx, o.cancel = context.WithCancel(parent)
	ctx := o.ctx
	o.mu.Unlock()
	var group sync.WaitGroup
	defer func() {
		o.cancel()
		group.Wait()
		o.mu.Lock()
		pending := o.refreshing
		o.mu.Unlock()
		if pending != nil {
			<-pending.done
		}
		o.synced.Store(false)
		close(o.done)
	}()
	for _, informer := range o.informers {
		group.Add(1)
		go func() { defer group.Done(); informer.Run(ctx.Done()) }()
	}
	// Audit and the PG bridge must run even if a Watch's initial LIST is denied.
	group.Add(1)
	go func() { defer group.Done(); o.auditLoop(ctx) }()
	ticker := time.NewTicker(o.options.FlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			synced := true
			for _, i := range o.informers[:len(observationGVRs)] {
				synced = synced && i.HasSynced()
			}
			o.synced.Store(synced)
			o.flush(ctx)
		}
	}
}
func (o *KCObservation) Stop(ctx context.Context) error {
	o.mu.Lock()
	cancel, started := o.cancel, o.started
	o.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if !started {
		return nil
	}
	select {
	case <-o.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (o *KCObservation) auditLoop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		_, _ = o.refresh(ctx, time.Time{}, true)
		probe, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		metrics := o.provider.repository.observationTelemetry(probe)
		cancel()
		o.mu.Lock()
		o.pgMetrics = metrics
		o.mu.Unlock()
		delay := o.options.AuditInterval
		if o.options.AuditJitter > 0 {
			delay += time.Duration(rand.Int64N(int64(o.options.AuditJitter)))
		}
		timer.Reset(delay)
	}
}
func (o *KCObservation) flush(ctx context.Context) {
	o.mu.Lock()
	v := o.view
	if v == nil {
		o.mu.Unlock()
		return
	}
	keys := o.pending
	o.pending = map[string]struct{}{}
	o.pendingSince = time.Time{}
	overflow := o.overflow
	o.overflow = false
	o.mu.Unlock()
	targets := map[string]observationTarget{}
	if overflow {
		for _, ts := range v.targets {
			for _, t := range ts {
				targets[t.key()] = t
			}
		}
	} else {
		for key := range keys {
			for _, t := range v.targets[key] {
				targets[t.key()] = t
			}
		}
	}
	deadline, cancel := context.WithTimeout(ctx, o.options.AuditTimeout)
	defer cancel()
	for _, t := range targets {
		var err error
		if t.kind == "attachment" {
			err = o.provider.repository.NotifyAttachment(deadline, t.tenant, t.id)
		} else if t.tenant == "" {
			_, err = o.provider.repository.queries.NotifyPlatform(deadline, sqlcgen.NotifyPlatformParams{ResourceID: t.id})
		} else {
			err = o.provider.repository.NotifyResource(deadline, t.tenant, t.id)
		}
		if err != nil {
			o.mu.Lock()
			o.overflow = true
			o.mu.Unlock()
			return
		}
		o.notifications.Add(1)
	}
}

// refresh coalesces complete paginated audits. Caller cancellation never cancels
// another worker's collection; all work remains bounded by the lifecycle timeout.
func (o *KCObservation) refresh(ctx context.Context, after time.Time, force bool) (*auditView, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		o.mu.Lock()
		if !force && o.view != nil && !o.view.collected.Before(after) {
			v := o.view
			o.mu.Unlock()
			return v, nil
		}
		current := o.refreshing
		if current == nil {
			current = &auditCollection{done: make(chan struct{})}
			o.refreshing = current
			o.collectingSince = time.Now()
			lifecycle := o.ctx
			if lifecycle == nil {
				lifecycle = ctx
			}
			go func() {
				collect, cancel := context.WithTimeout(lifecycle, o.options.AuditTimeout)
				defer cancel()
				view, err := o.collect(collect)
				o.mu.Lock()
				elapsed := time.Since(o.collectingSince).Seconds()
				o.collectingSince = time.Time{}
				if err != nil {
					o.auditFailures.Add(1)
				} else {
					o.auditMaxSeconds = max(o.auditMaxSeconds, elapsed)
					o.view = view
					o.auditSuccess.Store(time.Now().UnixNano())
				}
				current.view, current.err = view, err
				o.refreshing = nil
				close(current.done)
				o.mu.Unlock()
			}()
		}
		o.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-current.done:
		}
		if current.err != nil {
			return nil, current.err
		}
		if current.view != nil && !current.view.collected.Before(after) {
			return current.view, nil
		}
		// A critical caller can join an audit begun before its own boundary.
		// Keep that fixed boundary and await the next shared collection here.
		// Returning a retry would move the boundary on every new Claim and can
		// starve cleanup while other workers continually keep an audit in flight.
		force = false
	}
}
func (o *KCObservation) collect(ctx context.Context) (*auditView, error) {
	o.mu.Lock()
	seq, epoch := o.sequence, o.epoch
	o.mu.Unlock()
	// Capture pending generations before collection. A concurrent notification
	// cannot be completed by a view whose collection did not cover that trigger.
	resources, err := o.provider.repository.queries.ObservationResources(ctx, sqlcgen.ObservationResourcesParams{ClusterID: o.provider.repository.placement.ClusterID})
	if err != nil {
		return nil, err
	}
	platforms, err := o.provider.repository.queries.ObservationPlatformResources(ctx, sqlcgen.ObservationPlatformResourcesParams{ClusterID: o.provider.repository.placement.ClusterID})
	if err != nil {
		return nil, err
	}
	attachments, err := o.provider.repository.queries.ObservationAttachments(ctx, sqlcgen.ObservationAttachmentsParams{ClusterID: o.provider.repository.placement.ClusterID})
	if err != nil {
		return nil, err
	}
	now, err := o.provider.repository.queries.DatabaseTime(ctx)
	if err != nil {
		return nil, err
	}
	v := &auditView{collected: now, sequence: seq, epoch: epoch, indices: map[schema.GroupVersionResource]cache.Indexer{}, generations: map[string]int64{}, targets: map[string][]observationTarget{}}
	for _, gvr := range observationGVRs {
		objects, err := o.provider.listAll(ctx, gvr)
		if err != nil {
			return nil, err
		}
		index := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{relationshipIndex: relationshipKeys})
		for _, object := range objects {
			obj := object.DeepCopy()
			if err = index.Add(obj); err != nil {
				return nil, err
			}
			body, _ := json.Marshal(obj.Object)
			v.bytes += int64(len(body))
		}
		v.indices[gvr] = index
	}
	if err = o.collectLB(ctx, v); err != nil {
		return nil, err
	}
	bind := func(keys []string, t observationTarget) {
		for _, k := range keys {
			v.targets[k] = append(v.targets[k], t)
		}
	}
	subnetRefs := map[string]string{}
	for _, r := range resources {
		t := observationTarget{r.TenantID, r.ResourceID, r.ResourceKind}
		v.generations[t.key()] = r.RequestedGeneration
		kind, field := "VPC", "gateway"
		if r.ResourceKind == "subnet" {
			kind, field = "Subnet", "subnet"
			subnetRefs[r.ResourceID] = r.Namespace + "/" + r.ProviderName
		}
		if r.ResourceKind == "eip" || r.ResourceKind == "snat" {
			kind = egressCRKind(r.ResourceKind)
			field = r.ResourceKind
		}
		if r.ResourceKind == "load_balancer" {
			kind, field = "Gateway", "load-balancer"
		}

		bind([]string{"object:" + kind + "/" + r.Namespace + "/" + r.ProviderName, field + ":" + r.Namespace + "/" + r.ProviderName, "uid:" + r.ProviderUid}, t)
	}
	for _, r := range platforms {
		t := observationTarget{"", r.ResourceID, r.Kind}
		v.generations[t.key()] = r.RequestedGeneration
		ns := ""
		if r.Kind == "public_pool" {
			ns = kcSystemNamespace
		}
		relation := r.Kind
		if relation == "public_pool" {
			relation = "pool"
		}
		if relation == "egress_gateway" {
			relation = "gateway"
		}
		keys := []string{"object:" + egressCRKind(r.Kind) + "/" + ns + "/" + r.ProviderName, relation + ":" + ns + "/" + r.ProviderName, "uid:" + r.ProviderUid}
		if r.Kind == "device" || r.Kind == "vlan" {
			keys = append(keys, "node-facts")
		}

		if r.Kind == "public_pool" {
			keys = append(keys, "provider-images", "object:ConfigMap/kcn-system/kcn-config", "intranet-service-ranges")
		}
		bind(keys, t)
	}
	edges, err := o.provider.repository.queries.ObservationEgressEdges(ctx, sqlcgen.ObservationEgressEdgesParams{ClusterID: o.provider.repository.placement.ClusterID})
	if err != nil {
		return nil, err
	}
	for _, edge := range edges {
		relation := edge.RefKind
		if relation == "public_pool" {
			relation = "pool"
		}
		if relation == "egress_gateway" {
			relation = "gateway"
		}
		bind([]string{"node-facts", "provider-images", "object:ConfigMap/kcn-system/kcn-config", "intranet-service-ranges", "object:" + egressCRKind(edge.RefKind) + "/" + edge.Namespace + "/" + edge.ProviderName, relation + ":" + edge.Namespace + "/" + edge.ProviderName, "uid:" + edge.ProviderUid}, observationTarget{edge.TenantID, edge.ResourceID, edge.Kind})
	}
	for _, a := range attachments {
		t := observationTarget{a.TenantID, a.AttachmentID, "attachment"}
		v.generations[t.key()] = a.RequestedGeneration
		keys := []string{"attachment:" + a.AttachmentID, "uid:" + a.PodUid, "podname:" + a.Namespace + "/" + a.PodName, "object:Pod/" + a.Namespace + "/" + a.PodName, "eip-subnet:" + subnetRefs[a.SubnetID]}
		var prior []attachmentRelation
		if err := json.Unmarshal(a.ProviderRelations, &prior); err != nil {
			return nil, err
		}
		for _, r := range prior {
			keys = append(keys, "uid:"+r.UID, "uid:"+r.OwnerUID, "object:"+r.Kind+"/"+r.Namespace+"/"+r.Name, "vNic:"+r.Namespace+"/"+r.Name)
		}
		for _, gvr := range observationGVRs[2:5] {
			for _, obj := range indexedObjects(v.indices[gvr], keys) {
				keys = append(keys, "uid:"+string(obj.GetUID()))
				if obj.GetKind() == "VNic" {
					keys = append(keys, "vNic:"+obj.GetNamespace()+"/"+obj.GetName())
				}
			}
		}
		bind(keys, t)
	}
	lbEdges, err := o.provider.repository.queries.ObservationLBEdges(ctx, sqlcgen.ObservationLBEdgesParams{ClusterID: o.provider.repository.placement.ClusterID})
	if err != nil {
		return nil, err
	}
	for _, edge := range lbEdges {
		bind([]string{edge.RelationKey, "lb-capability"}, observationTarget{edge.TenantID, edge.LbID, "load_balancer"})
	}
	return v, nil
}

// Attachment observations consume only Pod/VNic/VNicIP/EIP facts. A sibling
// Service can share a Notebook owner UID without changing any of those facts.
// Retain general changes for Network/LB and the same overflow/continuity epoch.
func attachmentObservationObject(object *unstructured.Unstructured) bool {
	if object.GetAPIVersion() == "v1" && object.GetKind() == "Pod" {
		return true
	}
	if object.GetAPIVersion() != "networking.kubercloud.com/v1" {
		return false
	}
	switch object.GetKind() {
	case "VNic", "VNicIP", "EIP":
		return true
	}
	return false
}

func (o *KCObservation) valid(v *auditView, keys []string) bool {
	return o.validChanges(v, keys, false)
}
func (o *KCObservation) validAttachment(v *auditView, keys []string) bool {
	return o.validChanges(v, keys, true)
}
func (o *KCObservation) validChanges(v *auditView, keys []string, attachment bool) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if v.epoch != o.epoch {
		return false
	}
	changes := o.changes
	if attachment {
		changes = o.attachmentChanges
	}
	for _, key := range keys {
		if changes[key] > v.sequence {
			return false
		}
	}
	return true
}
func (o *KCObservation) snapshot(ctx context.Context, after time.Time) (*auditView, error) {
	if after.IsZero() {
		after = time.Now().Add(-o.options.AuditInterval - o.options.AuditJitter)
	}
	return o.refresh(ctx, after, false)
}

// Snapshot exposes bounded operational measurements; no tenant/object label is
// emitted. Encoded byte sizes estimate index payload, not allocator RSS.
func (o *KCObservation) Snapshot() map[string]float64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	m := o.provider.io.snapshot()
	m["watch_index_payload_bytes"] = float64(max(0, o.watchIndexBytes.Load()))
	m["audit_collection_max_seconds"] = o.auditMaxSeconds
	m["audit_failures_total"] = float64(o.auditFailures.Load())
	if !o.collectingSince.IsZero() {
		m["audit_collecting_seconds"] = time.Since(o.collectingSince).Seconds()
	}
	for k, v := range o.pgMetrics {
		m[k] = v
	}
	for name, value := range map[string]float64{"source_synced": 0, "pending_keys": float64(len(o.pending)), "watch_errors_total": float64(o.watchErrors.Load()), "source_boundaries_total": float64(o.sourceStarts.Load()), "notifications_total": float64(o.notifications.Load()), "coalesced_total": float64(o.coalesced.Load()), "queue_overflows_total": float64(o.overflows.Load()), "relation_candidates_total": float64(o.candidates.Load())} {
		m[name] = value
	}
	if !o.pendingSince.IsZero() {
		m["oldest_memory_hint_seconds"] = time.Since(o.pendingSince).Seconds()
	}
	if o.synced.Load() {
		m["source_synced"] = 1
	}
	if v := o.view; v != nil {
		m["audit_evidence_age_seconds"] = time.Since(v.collected).Seconds()
		m["audit_index_payload_bytes"] = float64(v.bytes)
		for gvr, index := range v.indices {
			m["audit_objects_"+gvr.Resource] = float64(len(index.ListKeys()))
		}
	}
	return m
}
