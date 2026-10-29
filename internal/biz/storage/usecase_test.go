package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

const (
	testTenant  = "11111111-1111-4111-8111-111111111111"
	otherTenant = "22222222-2222-4222-8222-222222222222"
	testPrefix  = "tenant-"
	testNS      = "tenant-11111111-1111-4111-8111-111111111111"
)

type fakeVolumes struct {
	namespace    Namespace
	namespaceErr error
	records      map[string]*VolumeRecord
	classes      map[string]Class
	createErr    error
	resizeErr    error
	deleteErr    error
	pods         []Consumer
	podErr       error
	notebooks    []Consumer
	notebookErr  error
	events       []Event
	eventErr     error
	pv           *PVRecord
}

func newFake() *fakeVolumes {
	return &fakeVolumes{
		namespace: Namespace{Name: testNS, ManagedBy: NamespaceOwner, TenantID: testTenant},
		records:   map[string]*VolumeRecord{},
		classes:   map[string]Class{"fast": {Name: "fast", AllowVolumeExpansion: true, ReclaimPolicy: "Delete"}},
	}
}

func (f *fakeVolumes) GetNamespace(context.Context, string) (Namespace, error) {
	return f.namespace, f.namespaceErr
}

func (f *fakeVolumes) GetVolume(_ context.Context, _, name string) (*VolumeRecord, error) {
	record, ok := f.records[name]
	if !ok {
		return nil, ErrNotFound
	}
	copy := *record
	return &copy, nil
}

func (f *fakeVolumes) CreateVolume(_ context.Context, _ string, spec CreateSpec) (*VolumeRecord, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	record := &VolumeRecord{
		Name: spec.Name, UID: uuid.NewString(), DisplayName: spec.DisplayName,
		StorageClassName: spec.StorageClassName, VolumeMode: spec.VolumeMode,
		AccessModes: spec.AccessModes, RequestedBytes: spec.RequestedBytes,
		RequestedQuantity: spec.RequestedQuantity, ResourceVersion: "1",
		Labels: spec.Labels, Annotations: spec.Annotations, Phase: "Pending",
	}
	f.records[spec.Name] = record
	copy := *record
	return &copy, nil
}

func (f *fakeVolumes) ListVolumes(context.Context, string, string) ([]*VolumeRecord, error) {
	out := make([]*VolumeRecord, 0, len(f.records))
	for _, record := range f.records {
		out = append(out, record)
	}
	return out, nil
}

func (f *fakeVolumes) ResizeVolume(_ context.Context, _, name, expectedUID, expectedRV, quantity string, capacityBytes int64) (*VolumeRecord, error) {
	if f.resizeErr != nil {
		return nil, f.resizeErr
	}
	record := f.records[name]
	if record.UID != expectedUID {
		return nil, ErrIdentityChanged
	}
	if record.ResourceVersion != expectedRV {
		return nil, ErrConflict
	}
	record.RequestedBytes = capacityBytes
	record.RequestedQuantity = quantity
	record.ResourceVersion = "2"
	copy := *record
	return &copy, nil
}

func (f *fakeVolumes) DeleteVolume(_ context.Context, _, name, _ string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.records, name)
	return nil
}

func (f *fakeVolumes) ListStorageClasses(context.Context) ([]Class, error) {
	out := make([]Class, 0, len(f.classes))
	for _, class := range f.classes {
		out = append(out, class)
	}
	return out, nil
}

func (f *fakeVolumes) GetStorageClass(_ context.Context, name string) (*Class, error) {
	class, ok := f.classes[name]
	if !ok {
		return nil, ErrNotFound
	}
	return &class, nil
}

func (f *fakeVolumes) GetPersistentVolume(context.Context, string) (*PVRecord, error) {
	if f.pv == nil {
		return nil, ErrNotFound
	}
	return f.pv, nil
}

func (f *fakeVolumes) ListPodConsumers(_ context.Context, _, claimName string) ([]Consumer, error) {
	out := make([]Consumer, 0, len(f.pods))
	for _, consumer := range f.pods {
		if consumer.Claim == claimName {
			out = append(out, consumer)
		}
	}
	return out, f.podErr
}

func (f *fakeVolumes) ListNotebookConsumers(_ context.Context, _, claimName string) ([]Consumer, error) {
	out := make([]Consumer, 0, len(f.notebooks))
	for _, consumer := range f.notebooks {
		if consumer.Claim == claimName {
			out = append(out, consumer)
		}
	}
	return out, f.notebookErr
}

func (f *fakeVolumes) ListEvents(context.Context, string, string) ([]Event, error) {
	return f.events, f.eventErr
}

type fakeObjects struct {
	available bool
	page      ObjectPage
	headErr   error
}

func (f *fakeObjects) Available() bool { return f.available }
func (f *fakeObjects) ListObjects(context.Context, string, string, string, int32) (ObjectPage, error) {
	return f.page, nil
}
func (f *fakeObjects) HeadObject(context.Context, string, string) (ObjectMeta, error) {
	if f.headErr != nil {
		return ObjectMeta{}, f.headErr
	}
	return ObjectMeta{Key: "a/b.txt", Size: 3}, nil
}
func (f *fakeObjects) PresignGetObject(context.Context, string, string, time.Duration) (string, time.Time, error) {
	return "https://browser.example/b/k", time.Now().Add(time.Minute), nil
}

func newUseCase(t *testing.T, volumes VolumePort, objects ObjectPort) *UseCase {
	t.Helper()
	uc, err := NewUseCase(volumes, objects, Config{NamespacePrefix: testPrefix, ClusterID: testTenant, DeleteExecutor: "storage-tests", BorrowedPVCGuard: newTestDeleteGuard(), Buckets: []BucketConfig{
		{ID: "files", TenantID: testTenant, Name: "文件", Bucket: "tenant-files"},
		{ID: "other", TenantID: otherTenant, Name: "别人", Bucket: "other-files"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return uc
}

func createRequest(source string) CreateRequest {
	return CreateRequest{CreateToken: source, DisplayName: "数据盘", CapacityGB: 20, StorageClassName: "fast"}
}

func TestCapacityConversion(t *testing.T) {
	if bytes, err := CapacityBytes(20); err != nil || bytes != 20_000_000_000 {
		t.Fatalf("20GB = %d, %v", bytes, err)
	}
	for _, value := range []int64{0, -1, -20, 1 << 62} {
		if _, err := CapacityBytes(value); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%d must be rejected", value)
		}
	}
}

func TestDigestDefaultsAreStable(t *testing.T) {
	implicit, err := NormalizeRequest(uuid.NewString(), "盘", 20, "fast", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	explicit, err := NormalizeRequest(uuid.NewString(), "盘", 20, "fast", ModeFilesystem, []string{"ReadWriteOnce"})
	if err != nil {
		t.Fatal(err)
	}
	if RequestDigest(implicit) != RequestDigest(explicit) {
		t.Fatal("default and explicit default must produce the same digest")
	}
	changed, _ := NormalizeRequest(uuid.NewString(), "盘", 20, "fast", ModeFilesystem, []string{"ReadWriteMany"})
	if RequestDigest(changed) == RequestDigest(implicit) {
		t.Fatal("different access modes must change the digest")
	}
}

func TestCreateReplayOnlyReturnsSameVolume(t *testing.T) {
	volumes := newFake()
	uc := newUseCase(t, volumes, nil)
	request := createRequest(uuid.NewString())
	first, err := uc.CreateVolume(context.Background(), testTenant, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := uc.CreateVolume(context.Background(), testTenant, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.UID != second.UID || second.Name != first.Name {
		t.Fatal("same token and content must return the same PVC")
	}
	changed := request
	changed.CapacityGB = 30
	if _, err := uc.CreateVolume(context.Background(), testTenant, changed); !errors.Is(err, ErrCreateConflict) {
		t.Fatalf("different content must conflict, got %v", err)
	}
	if _, err := uc.CreateVolume(context.Background(), otherTenant, createRequest(uuid.NewString())); !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrCreateConflict) {
		t.Fatalf("namespace owned by another tenant must be hidden, got %v", err)
	}
}

func TestCreateRequiresStorageClass(t *testing.T) {
	volumes := newFake()
	uc := newUseCase(t, volumes, nil)
	request := createRequest(uuid.NewString())
	request.StorageClassName = "missing"
	if _, err := uc.CreateVolume(context.Background(), testTenant, request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing StorageClass must be invalid, got %v", err)
	}
}

func TestResizeBoundaries(t *testing.T) {
	ctx := context.Background()
	volumes := newFake()
	uc := newUseCase(t, volumes, nil)
	request := createRequest(uuid.NewString())
	created, err := uc.CreateVolume(ctx, testTenant, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.UpdateVolume(ctx, testTenant, created.Name, "old-uid", 30); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("old uid must be rejected, got %v", err)
	}
	if _, err := uc.UpdateVolume(ctx, testTenant, created.Name, created.UID, 10); !errors.Is(err, ErrCapacityDecrease) {
		t.Fatalf("shrink must be rejected, got %v", err)
	}
	same, err := uc.UpdateVolume(ctx, testTenant, created.Name, created.UID, 20)
	if err != nil || same.RequestedCapacityGB != 20 {
		t.Fatalf("same capacity must return current, got %v", err)
	}
	volumes.classes["full"] = Class{Name: "full", AllowVolumeExpansion: false}
	volumes.records[created.Name].StorageClassName = "full"
	if _, err := uc.UpdateVolume(ctx, testTenant, created.Name, created.UID, 30); !errors.Is(err, ErrExpansionDenied) {
		t.Fatalf("class without expansion must be rejected, got %v", err)
	}
	volumes.records[created.Name].StorageClassName = "fast"
	resized, err := uc.UpdateVolume(ctx, testTenant, created.Name, created.UID, 30)
	if err != nil || resized.RequestedCapacityGB != 30 {
		t.Fatalf("resize failed: %v", err)
	}
}

func TestDeleteUsageAndIdentity(t *testing.T) {
	ctx := context.Background()
	volumes := newFake()
	uc := newUseCase(t, volumes, nil)
	created, err := uc.CreateVolume(ctx, testTenant, createRequest(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uc.DeleteVolume(ctx, testTenant, created.Name, "old-uid"); !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("old uid must not delete a new object, got %v", err)
	}
	volumes.pods = []Consumer{{Kind: "Pod", Namespace: testNS, Name: "worker", Claim: created.Name}}
	if _, err := uc.DeleteVolume(ctx, testTenant, created.Name, created.UID); !errors.Is(err, ErrVolumeInUse) {
		t.Fatalf("in-use volume must be rejected, got %v", err)
	}
	volumes.pods = nil
	volumes.podErr = errors.New("forbidden")
	if _, err := uc.DeleteVolume(ctx, testTenant, created.Name, created.UID); !errors.Is(err, ErrUsageUnknown) {
		t.Fatalf("usage query failure must not delete, got %v", err)
	}
	volumes.podErr = nil
	result, err := uc.DeleteVolume(ctx, testTenant, created.Name, created.UID)
	if err != nil || result.State != DeletionGone {
		t.Fatalf("delete result = %v, %v", result.State, err)
	}
	result, err = uc.DeleteVolume(ctx, testTenant, created.Name, created.UID)
	if err != nil || result.State != DeletionGone {
		t.Fatalf("already-gone delete = %v, %v", result.State, err)
	}
}

func TestDeleteIgnoresConsumersOfOtherClaims(t *testing.T) {
	ctx := context.Background()
	volumes := newFake()
	uc := newUseCase(t, volumes, nil)
	busy, err := uc.CreateVolume(ctx, testTenant, createRequest(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	idle, err := uc.CreateVolume(ctx, testTenant, createRequest(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	// 同命名空间的工作负载只引用 busy，不应阻止 idle 删除。
	volumes.pods = []Consumer{{Kind: "Pod", Namespace: testNS, Name: "worker", Claim: busy.Name}}
	result, err := uc.DeleteVolume(ctx, testTenant, idle.Name, idle.UID)
	if err != nil || result.State != DeletionGone {
		t.Fatalf("idle volume must be deletable, got %v, %v", result.State, err)
	}
	// busy 仍不应被删除。
	if _, err := uc.DeleteVolume(ctx, testTenant, busy.Name, busy.UID); !errors.Is(err, ErrVolumeInUse) {
		t.Fatalf("referenced volume must stay protected, got %v", err)
	}
	// 详情只回显引用该 PVC 的使用方。
	detail, err := uc.GetVolume(ctx, testTenant, busy.Name)
	if err != nil || len(detail.Consumers) != 1 || detail.Consumers[0].Name != "worker" {
		t.Fatalf("detail consumers = %+v, %v", detail.Consumers, err)
	}
	idleDetail, err := uc.GetVolume(ctx, testTenant, idle.Name)
	if err == nil && len(idleDetail.Consumers) != 0 {
		t.Fatalf("idle detail must not report consumers: %+v", idleDetail.Consumers)
	}
}

func TestVolumeOperationsRequireNamespaceOwnership(t *testing.T) {
	ctx := context.Background()
	volumes := newFake()
	uc := newUseCase(t, volumes, nil)
	created, err := uc.CreateVolume(ctx, testTenant, createRequest(uuid.NewString()))
	if err != nil {
		t.Fatal(err)
	}
	for name, namespace := range map[string]Namespace{
		"label-mismatch": {Name: testNS, ManagedBy: "other", TenantID: testTenant},
		"deleting":       {Name: testNS, ManagedBy: NamespaceOwner, TenantID: testTenant, Deleting: true},
	} {
		volumes.namespace = namespace
		if _, _, err := uc.ListVolumes(ctx, testTenant, 1, 20, VolumeFilter{}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: list must be hidden, got %v", name, err)
		}
		if _, err := uc.GetVolume(ctx, testTenant, created.Name); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: detail must be hidden, got %v", name, err)
		}
		if _, err := uc.UpdateVolume(ctx, testTenant, created.Name, created.UID, 30); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: resize must be hidden, got %v", name, err)
		}
		if _, err := uc.DeleteVolume(ctx, testTenant, created.Name, created.UID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s: delete must be hidden, got %v", name, err)
		}
	}
}

func TestListVolumesIsStableAndScoped(t *testing.T) {
	ctx := context.Background()
	volumes := newFake()
	uc := newUseCase(t, volumes, nil)
	for index := 0; index < 3; index++ {
		if _, err := uc.CreateVolume(ctx, testTenant, createRequest(uuid.NewString())); err != nil {
			t.Fatal(err)
		}
	}
	foreign := &VolumeRecord{Name: "foreign", UID: "x", Labels: map[string]string{LabelManagedBy: ManagedByValue, LabelTenantID: otherTenant}}
	volumes.records["foreign"] = foreign
	items, total, err := uc.ListVolumes(ctx, testTenant, 1, 2, VolumeFilter{})
	if err != nil || total != 3 || len(items) != 2 {
		t.Fatalf("page = %d/%d, %v", len(items), total, err)
	}
	if items[0].Name > items[1].Name {
		t.Fatal("volumes must be sorted")
	}
	_, _, err = uc.ListVolumes(ctx, testTenant, 1, 101, VolumeFilter{})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized page must be rejected, got %v", err)
	}
}

func TestListVolumesFilters(t *testing.T) {
	ctx := context.Background()
	volumes := newFake()
	uc := newUseCase(t, volumes, nil)
	add := func(name, uid, display, phase string) {
		volumes.records[name] = &VolumeRecord{
			Name: name, UID: uid, DisplayName: display, Phase: phase,
			Labels: map[string]string{LabelManagedBy: ManagedByValue, LabelTenantID: testTenant},
		}
	}
	add("ani-vol-a", "uid-a", "数据盘A", "Pending")
	add("ani-vol-b", "uid-b", "DATA disk", "Bound")
	add("ani-vol-c", "uid-c", "日志盘", "Bound")

	cases := []struct {
		name   string
		filter VolumeFilter
		page   uint32
		size   uint32
		total  int64
		keys   []string
	}{
		{"name contains", VolumeFilter{Name: "数据"}, 1, 20, 1, []string{"ani-vol-a"}},
		{"name case insensitive", VolumeFilter{Name: "data"}, 1, 20, 1, []string{"ani-vol-b"}},
		{"id matches pvc name", VolumeFilter{ID: "ani-vol-b"}, 1, 20, 1, []string{"ani-vol-b"}},
		{"id matches uid", VolumeFilter{ID: "uid-c"}, 1, 20, 1, []string{"ani-vol-c"}},
		{"id is exact", VolumeFilter{ID: "ani-vol"}, 1, 20, 0, []string{}},
		{"state matches phase", VolumeFilter{State: "Bound"}, 1, 20, 2, []string{"ani-vol-b", "ani-vol-c"}},
		{"combined filter", VolumeFilter{Name: "data", State: "Bound"}, 1, 20, 1, []string{"ani-vol-b"}},
		{"filtered second page", VolumeFilter{State: "Bound"}, 2, 1, 2, []string{"ani-vol-c"}},
	}
	for _, tc := range cases {
		items, total, err := uc.ListVolumes(ctx, testTenant, tc.page, tc.size, tc.filter)
		if err != nil || total != tc.total || len(items) != len(tc.keys) {
			t.Fatalf("%s: total = %d/%d, items = %d, err = %v", tc.name, total, tc.total, len(items), err)
		}
		for index, expected := range tc.keys {
			if items[index].Name != expected {
				t.Fatalf("%s: items[%d] = %s, want %s", tc.name, index, items[index].Name, expected)
			}
		}
	}

	for _, filter := range []VolumeFilter{
		{State: "bound"},
		{State: "Unknown"},
		{Name: "   "},
		{ID: "  "},
		{Name: strings.Repeat("盘", 129)},
		{ID: strings.Repeat("a", 254)},
	} {
		if _, _, err := uc.ListVolumes(ctx, testTenant, 1, 20, filter); !errors.Is(err, ErrInvalid) {
			t.Fatalf("filter %+v must be rejected, got %v", filter, err)
		}
	}
}

func TestListBucketsFilters(t *testing.T) {
	uc := newUseCase(t, newFake(), nil)
	cases := []struct {
		filter BucketFilter
		total  int
	}{
		{BucketFilter{}, 1},
		{BucketFilter{Name: "文"}, 1},
		{BucketFilter{Name: "文", ID: "files"}, 1},
		{BucketFilter{Name: "xyz"}, 0},
		{BucketFilter{ID: "file"}, 0},
	}
	for _, tc := range cases {
		items, err := uc.ListBuckets(testTenant, tc.filter)
		if err != nil || len(items) != tc.total {
			t.Fatalf("filter %+v: items = %d/%d, err = %v", tc.filter, len(items), tc.total, err)
		}
	}
	for _, filter := range []BucketFilter{{Name: "  "}, {ID: "  "}, {Name: strings.Repeat("桶", 129)}, {ID: strings.Repeat("a", 65)}} {
		if _, err := uc.ListBuckets(testTenant, filter); !errors.Is(err, ErrInvalid) {
			t.Fatalf("filter %+v must be rejected, got %v", filter, err)
		}
	}
}

func TestCursorBinding(t *testing.T) {
	cursor := EncodeCursor("files", "a/", "s3-token")
	token, err := DecodeCursor(cursor, "files", "a/")
	if err != nil || token != "s3-token" {
		t.Fatalf("roundtrip = %q, %v", token, err)
	}
	if _, err := DecodeCursor(cursor, "other", "a/"); !errors.Is(err, ErrInvalid) {
		t.Fatal("bucket mismatch must fail")
	}
	if _, err := DecodeCursor(cursor, "files", "b/"); !errors.Is(err, ErrInvalid) {
		t.Fatal("prefix mismatch must fail")
	}
}

func TestObjectLimitAndBucketIsolation(t *testing.T) {
	ctx := context.Background()
	objects := &fakeObjects{available: true, page: ObjectPage{Items: []ObjectItem{{Key: "a"}}, IsTruncated: true, NextToken: "next"}}
	uc := newUseCase(t, newFake(), objects)
	page, err := uc.ListObjects(ctx, testTenant, "files", "", false, 0, "")
	if err != nil || page.NextToken == "" {
		t.Fatalf("default limit must pass and encode cursor: %v", err)
	}
	if _, err := uc.ListObjects(ctx, testTenant, "files", "", true, 0, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("explicit zero limit must fail")
	}
	if _, err := uc.ListObjects(ctx, testTenant, "files", "", true, 101, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("limit above 100 must fail")
	}
	if _, err := uc.ListObjects(ctx, testTenant, "other", "", false, 0, ""); !errors.Is(err, ErrNotFound) {
		t.Fatal("another tenant's bucket must be hidden")
	}
	if _, err := uc.HeadObject(ctx, testTenant, "files", ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("empty key must fail")
	}
	if _, _, err := uc.DownloadURL(ctx, testTenant, "files", "a"); err != nil {
		t.Fatalf("download url: %v", err)
	}
	objects.available = false
	if _, _, err := uc.DownloadURL(ctx, testTenant, "files", "a"); !errors.Is(err, ErrDownloadUnavailable) {
		t.Fatal("missing browser endpoint must report DOWNLOAD_ENDPOINT_UNAVAILABLE")
	}
}
