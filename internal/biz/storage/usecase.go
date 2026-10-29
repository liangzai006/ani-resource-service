package storage

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"
)

const (
	defaultPageSize    = 20
	maximumPageSize    = 100
	defaultLimit       = 100
	maximumLimit       = 100
	defaultDownloadTTL = 10 * time.Minute
)

// Config 是 Storage 用例的静态配置；新增租户桶需要改配置并重启。
type Config struct {
	NamespacePrefix    string
	ClusterID          string
	DeleteExecutor     string
	BorrowedPVCGuard   BorrowedPVCGuard
	ObserveDeleteWrite func(string)
	Buckets            []BucketConfig
	DownloadTTL        time.Duration
}

// UseCase 编排独立盘与只读对象用例，不直接依赖 Kratos、Proto 或 SDK。
type UseCase struct {
	volumes VolumePort
	objects ObjectPort
	cfg     Config
	now     func() time.Time
}

func NewUseCase(volumes VolumePort, objects ObjectPort, cfg Config) (*UseCase, error) {
	if volumes == nil || cfg.NamespacePrefix == "" {
		return nil, ErrInvalid
	}
	if err := ValidateBuckets(cfg.Buckets); err != nil {
		return nil, err
	}
	if cfg.DownloadTTL <= 0 || cfg.DownloadTTL > defaultDownloadTTL {
		cfg.DownloadTTL = defaultDownloadTTL
	}
	return &UseCase{volumes: volumes, objects: objects, cfg: cfg, now: time.Now}, nil
}

func (u *UseCase) namespace(tenant string) (string, error) {
	return NamespaceName(u.cfg.NamespacePrefix, tenant)
}

// ensureNamespace 校验租户命名空间归属与状态，并返回命名空间名。
// 现有卷的列表、详情、扩容、删除都经过这里，避免命名空间标签失效或进入删除中后继续操作。
func (u *UseCase) ensureNamespace(ctx context.Context, tenant string) (string, error) {
	if !CanonicalUUID(tenant) {
		return "", ErrInvalid
	}
	name, err := u.namespace(tenant)
	if err != nil {
		return "", err
	}
	ns, err := u.volumes.GetNamespace(ctx, name)
	if err != nil {
		return "", err
	}
	if ns.Deleting || ns.ManagedBy != NamespaceOwner || ns.TenantID != tenant {
		return "", ErrNotFound
	}
	return name, nil
}

func owned(record *VolumeRecord, tenant string) bool {
	if record == nil {
		return false
	}
	return record.Labels[LabelManagedBy] == ManagedByValue && record.Labels[LabelTenantID] == tenant
}

// ListStorageClasses 返回全部可选 StorageClass，排序与分页由调用方负责。
func (u *UseCase) ListStorageClasses(ctx context.Context, tenant string) ([]Class, error) {
	if !CanonicalUUID(tenant) {
		return nil, ErrInvalid
	}
	items, err := u.volumes.ListStorageClasses(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// CreateVolume 使用 create_token 生成固定名称；只在未命中时执行首次创建前置检查。
func (u *UseCase) CreateVolume(ctx context.Context, tenant string, req CreateRequest) (Volume, error) {
	spec, err := NormalizeRequest(req.CreateToken, req.DisplayName, req.CapacityGB, req.StorageClassName, req.VolumeMode, req.AccessModes)
	if err != nil {
		return Volume{}, err
	}
	namespace, err := u.namespace(tenant)
	if err != nil {
		return Volume{}, err
	}
	if existing, err := u.volumes.GetVolume(ctx, namespace, spec.Name); err == nil {
		return u.replayCreate(tenant, existing, spec)
	} else if !errors.Is(err, ErrNotFound) {
		return Volume{}, err
	}
	if _, err := u.ensureNamespace(ctx, tenant); err != nil {
		return Volume{}, err
	}
	if _, err := u.volumes.GetStorageClass(ctx, spec.StorageClassName); err != nil {
		if errors.Is(err, ErrNotFound) {
			return Volume{}, ErrInvalid
		}
		return Volume{}, err
	}
	spec.Labels = map[string]string{
		LabelManagedBy:   ManagedByValue,
		LabelTenantID:    tenant,
		LabelCreateToken: req.CreateToken,
	}
	spec.Annotations = map[string]string{
		AnnotationName:   spec.DisplayName,
		AnnotationDigest: RequestDigest(spec),
	}
	record, err := u.volumes.CreateVolume(ctx, namespace, spec)
	if err == nil {
		return toVolume(record), nil
	}
	if !errors.Is(err, ErrConflict) {
		return Volume{}, err
	}
	// 查询与创建之间发生并发 AlreadyExists 时，按同一归属与摘要规则核对。
	existing, getErr := u.volumes.GetVolume(ctx, namespace, spec.Name)
	if getErr != nil {
		return Volume{}, getErr
	}
	return u.replayCreate(tenant, existing, spec)
}

func (u *UseCase) replayCreate(tenant string, existing *VolumeRecord, spec CreateSpec) (Volume, error) {
	if !owned(existing, tenant) || existing.Annotations[AnnotationDigest] != RequestDigest(spec) {
		return Volume{}, ErrCreateConflict
	}
	// 正在删除的同名对象仍返回其删除中状态，不恢复为新创建。
	return toVolume(existing), nil
}

// ListVolumes 按受管标签查询，筛选后稳定排序并分页；查询失败不返回空列表。
func (u *UseCase) ListVolumes(ctx context.Context, tenant string, page, pageSize uint32, filter VolumeFilter) ([]Volume, int64, error) {
	page, pageSize, offset, err := pageWindow(page, pageSize)
	if err != nil {
		return nil, 0, err
	}
	filter, err = NormalizeVolumeFilter(filter)
	if err != nil {
		return nil, 0, err
	}
	namespace, err := u.ensureNamespace(ctx, tenant)
	if err != nil {
		return nil, 0, err
	}
	records, err := u.volumes.ListVolumes(ctx, namespace, tenant)
	if err != nil {
		return nil, 0, err
	}
	filtered := make([]*VolumeRecord, 0, len(records))
	for _, record := range records {
		if owned(record, tenant) && matchVolumeFilter(record, filter) {
			filtered = append(filtered, record)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Name < filtered[j].Name })
	total := int64(len(filtered))
	if offset >= total {
		return []Volume{}, total, nil
	}
	end := offset + int64(pageSize)
	if end > total {
		end = total
	}
	out := make([]Volume, 0, end-offset)
	for _, record := range filtered[offset:end] {
		out = append(out, toVolume(record))
	}
	return out, total, nil
}

// GetVolume 返回 spec/status、conditions、事件、绑定 PV 回收策略与可观察使用方。
func (u *UseCase) GetVolume(ctx context.Context, tenant, name string) (VolumeDetail, error) {
	namespace, err := u.ensureNamespace(ctx, tenant)
	if err != nil {
		return VolumeDetail{}, err
	}
	record, err := u.volumes.GetVolume(ctx, namespace, name)
	if err != nil {
		return VolumeDetail{}, err
	}
	if !owned(record, tenant) {
		return VolumeDetail{}, ErrNotFound
	}
	consumers, err := u.usage(ctx, namespace, name)
	if err != nil {
		return VolumeDetail{}, err
	}
	events, err := u.volumes.ListEvents(ctx, namespace, record.UID)
	if err != nil {
		return VolumeDetail{}, err
	}
	volume := toVolume(record)
	volume.ReclaimPolicy, volume.ReclaimPolicyKnown = reclaim(record, func(name string) (*PVRecord, error) {
		return u.volumes.GetPersistentVolume(ctx, name)
	})
	return VolumeDetail{Volume: volume, Conditions: record.Conditions, Events: events, Consumers: consumers}, nil
}

func reclaim(record *VolumeRecord, get func(string) (*PVRecord, error)) (string, bool) {
	if record.PVName != "" {
		pv, err := get(record.PVName)
		if err != nil || pv == nil || pv.ReclaimPolicy == "" {
			return "", false
		}
		return pv.ReclaimPolicy, true
	}
	if record.ClassKnown && record.ClassReclaim != "" {
		return record.ClassReclaim, true
	}
	return "", false
}

// UpdateVolume 只允许扩容，且以 spec 当前申请值为比较基准。
func (u *UseCase) UpdateVolume(ctx context.Context, tenant, name, expectedUID string, capacityGB int64) (Volume, error) {
	bytes, err := CapacityBytes(capacityGB)
	if err != nil {
		return Volume{}, err
	}
	namespace, err := u.ensureNamespace(ctx, tenant)
	if err != nil {
		return Volume{}, err
	}
	record, err := u.volumes.GetVolume(ctx, namespace, name)
	if err != nil {
		return Volume{}, err
	}
	if !owned(record, tenant) {
		return Volume{}, ErrNotFound
	}
	if record.UID != expectedUID {
		return Volume{}, ErrIdentityChanged
	}
	if record.Deleting {
		return Volume{}, ErrVolumeTerminating
	}
	switch {
	case bytes == record.RequestedBytes:
		return toVolume(record), nil
	case bytes < record.RequestedBytes:
		return Volume{}, ErrCapacityDecrease
	}
	class, err := u.volumes.GetStorageClass(ctx, record.StorageClassName)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Volume{}, ErrExpansionDenied
		}
		return Volume{}, err
	}
	if !class.AllowVolumeExpansion {
		return Volume{}, ErrExpansionDenied
	}
	updated, err := u.volumes.ResizeVolume(ctx, namespace, name, expectedUID, record.ResourceVersion, strconv.FormatInt(capacityGB, 10)+"G", bytes)
	if err != nil {
		return Volume{}, err
	}
	return toVolume(updated), nil
}

// DeleteVolume reserves the durable name barrier before any Kubernetes DELETE.
func (u *UseCase) DeleteVolume(ctx context.Context, tenant, name, expectedUID string) (DeleteResult, error) {
	return u.deleteVolumeGuarded(ctx, tenant, name, expectedUID)
}

// usage 只统计引用目标 PVC 的 Pod 直接引用与 Notebook 模板引用；
// 同命名空间内引用其它 PVC 的工作负载不算使用方。
func (u *UseCase) usage(ctx context.Context, namespace, claimName string) ([]Consumer, error) {
	pods, err := u.volumes.ListPodConsumers(ctx, namespace, claimName)
	if err != nil {
		return nil, ErrUsageUnknown
	}
	notebooks, err := u.volumes.ListNotebookConsumers(ctx, namespace, claimName)
	if err != nil && !errors.Is(err, ErrNotInstalled) {
		return nil, ErrUsageUnknown
	}
	out := make([]Consumer, 0, len(pods)+len(notebooks))
	out = append(out, pods...)
	out = append(out, notebooks...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// ListBuckets 只返回配置给当前租户的桶，筛选后按稳定 ID 排序。
func (u *UseCase) ListBuckets(tenant string, filter BucketFilter) ([]Bucket, error) {
	filter, err := NormalizeBucketFilter(filter)
	if err != nil {
		return nil, err
	}
	out := []Bucket{}
	for _, b := range u.cfg.Buckets {
		if b.TenantID != tenant {
			continue
		}
		bucket := Bucket{ID: b.ID, Name: b.Name}
		if matchBucketFilter(bucket, filter) {
			out = append(out, bucket)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ListObjects 使用领域游标；limit 缺省 100，显式 0 或越界一律 400。
func (u *UseCase) ListObjects(ctx context.Context, tenant, bucketID, prefix string, limitSet bool, limit int, cursor string) (ObjectPage, error) {
	bucket, err := u.bucket(tenant, bucketID)
	if err != nil {
		return ObjectPage{}, err
	}
	if err := validatePrefix(prefix); err != nil {
		return ObjectPage{}, err
	}
	if limitSet {
		if limit < 1 || limit > maximumLimit {
			return ObjectPage{}, ErrInvalid
		}
	} else {
		limit = defaultLimit
	}
	token, err := DecodeCursor(cursor, bucketID, prefix)
	if err != nil {
		return ObjectPage{}, err
	}
	if u.objects == nil {
		return ObjectPage{}, ErrBackendUnavailable
	}
	page, err := u.objects.ListObjects(ctx, bucket.Bucket, prefix, token, int32(limit))
	if err != nil {
		return ObjectPage{}, err
	}
	if page.IsTruncated && page.NextToken != "" {
		page.NextToken = EncodeCursor(bucketID, prefix, page.NextToken)
	} else {
		page.NextToken = ""
	}
	return page, nil
}

// HeadObject 返回真实 HEAD 元数据；key 原样交给 SDK。
func (u *UseCase) HeadObject(ctx context.Context, tenant, bucketID, key string) (ObjectMeta, error) {
	bucket, err := u.bucket(tenant, bucketID)
	if err != nil {
		return ObjectMeta{}, err
	}
	if err := ValidateObjectKey(key); err != nil {
		return ObjectMeta{}, err
	}
	if u.objects == nil {
		return ObjectMeta{}, ErrBackendUnavailable
	}
	return u.objects.HeadObject(ctx, bucket.Bucket, key)
}

// DownloadURL 用后端凭据对浏览器 endpoint 生成固定对象的短期 GET URL。
func (u *UseCase) DownloadURL(ctx context.Context, tenant, bucketID, key string) (string, time.Time, error) {
	bucket, err := u.bucket(tenant, bucketID)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := ValidateObjectKey(key); err != nil {
		return "", time.Time{}, err
	}
	if u.objects == nil || !u.objects.Available() {
		return "", time.Time{}, ErrDownloadUnavailable
	}
	return u.objects.PresignGetObject(ctx, bucket.Bucket, key, u.cfg.DownloadTTL)
}

func (u *UseCase) bucket(tenant, id string) (BucketConfig, error) {
	if !CanonicalUUID(tenant) || id == "" {
		return BucketConfig{}, ErrInvalid
	}
	for _, b := range u.cfg.Buckets {
		if b.ID == id && b.TenantID == tenant {
			return b, nil
		}
	}
	return BucketConfig{}, ErrNotFound
}

func pageWindow(page, size uint32) (uint32, uint32, int64, error) {
	if page == 0 {
		page = 1
	}
	if size == 0 {
		size = defaultPageSize
	}
	if size > maximumPageSize {
		return 0, 0, 0, ErrInvalid
	}
	return page, size, int64(page-1) * int64(size), nil
}

func toVolume(record *VolumeRecord) Volume {
	gb := record.RequestedBytes / BytesPerGB
	if record.RequestedBytes > 0 && record.RequestedBytes%BytesPerGB != 0 {
		gb = GBFromBytes(record.RequestedBytes)
	}
	return Volume{
		Name:                  record.Name,
		UID:                   record.UID,
		DisplayName:           record.DisplayName,
		StorageClassName:      record.StorageClassName,
		VolumeMode:            record.VolumeMode,
		AccessModes:           append([]string(nil), record.AccessModes...),
		RequestedCapacityGB:   gb,
		ObservedCapacityBytes: record.ObservedBytes,
		ObservedCapacityKnown: record.ObservedKnown,
		ObservedQuantity:      record.ObservedQuantity,
		Phase:                 record.Phase,
		Deleting:              record.Deleting,
	}
}
