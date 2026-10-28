package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

var (
	dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	dnsLabel     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	bucketID     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

var accessModes = map[string]bool{
	"ReadWriteOnce":    true,
	"ReadOnlyMany":     true,
	"ReadWriteMany":    true,
	"ReadWriteOncePod": true,
}

// CanonicalUUID 只接受标准形态（小写、带连字符）的 UUID。
func CanonicalUUID(v string) bool {
	id, err := uuid.Parse(v)
	return err == nil && id != uuid.Nil && id.String() == v
}

// CapacityBytes 把正整数 GB 转为 bytes，拒绝零、负数和溢出。
func CapacityBytes(gb int64) (int64, error) {
	if gb <= 0 || gb > math.MaxInt64/BytesPerGB {
		return 0, ErrInvalid
	}
	return gb * BytesPerGB, nil
}

// GBFromBytes 把已接受的容量反算为 GB（仅用于展示，不改变用户申请值）。
func GBFromBytes(bytes int64) int64 {
	if bytes <= 0 {
		return 0
	}
	return bytes / BytesPerGB
}

// NamespaceName 按 NamespacePrefix + TenantID 推导，不接受任意 namespace 输入。
func NamespaceName(prefix, tenant string) (string, error) {
	if prefix == "" || !CanonicalUUID(tenant) {
		return "", ErrInvalid
	}
	name := prefix + tenant
	if len(name) > 63 || len(dnsLabel.FindString(name)) != len(name) {
		return "", ErrInvalid
	}
	return name, nil
}

// ValidateDisplayName 只校验页面名称；名称写入 annotation，不进入对象名。
func ValidateDisplayName(v string) error {
	name := strings.TrimSpace(v)
	if name == "" || utf8.RuneCountInString(name) > 128 {
		return ErrInvalid
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return ErrInvalid
		}
	}
	return nil
}

// NormalizeRequest 应用默认值并校验创建参数。
func NormalizeRequest(createToken, displayName string, capacityGB int64, class, mode string, modes []string) (CreateSpec, error) {
	if !CanonicalUUID(createToken) {
		return CreateSpec{}, ErrInvalid
	}
	if err := ValidateDisplayName(displayName); err != nil {
		return CreateSpec{}, err
	}
	bytes, err := CapacityBytes(capacityGB)
	if err != nil {
		return CreateSpec{}, err
	}
	if len(class) == 0 || len(class) > 253 || len(dnsSubdomain.FindString(class)) != len(class) {
		return CreateSpec{}, ErrInvalid
	}
	if mode == "" {
		mode = ModeFilesystem
	}
	if mode != ModeFilesystem && mode != ModeBlock {
		return CreateSpec{}, ErrInvalid
	}
	normalized, err := normalizeAccessModes(modes)
	if err != nil {
		return CreateSpec{}, err
	}
	return CreateSpec{
		Name:              "ani-vol-" + strings.ReplaceAll(createToken, "-", ""),
		DisplayName:       strings.TrimSpace(displayName),
		RequestedQuantity: fmt.Sprintf("%dG", capacityGB),
		RequestedBytes:    bytes,
		StorageClassName:  class,
		VolumeMode:        mode,
		AccessModes:       normalized,
	}, nil
}

func normalizeAccessModes(modes []string) ([]string, error) {
	if len(modes) == 0 {
		modes = []string{"ReadWriteOnce"}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(modes))
	for _, mode := range modes {
		if !accessModes[mode] || seen[mode] {
			return nil, ErrInvalid
		}
		seen[mode] = true
		out = append(out, mode)
	}
	sort.Strings(out)
	return out, nil
}

type digestPayload struct {
	DisplayName      string   `json:"display_name"`
	CapacityGB       int64    `json:"capacity_gb"`
	StorageClassName string   `json:"storage_class_name"`
	VolumeMode       string   `json:"volume_mode"`
	AccessModes      []string `json:"access_modes"`
}

// RequestDigest 对规范化后的原始创建参数求摘要。缺省值与显式默认值得到同一结果；
// 不包含 request_id、status、resourceVersion 或扩容后的容量。
func RequestDigest(spec CreateSpec) string {
	payload := digestPayload{
		DisplayName:      spec.DisplayName,
		CapacityGB:       spec.RequestedBytes / BytesPerGB,
		StorageClassName: spec.StorageClassName,
		VolumeMode:       spec.VolumeMode,
		AccessModes:      append([]string(nil), spec.AccessModes...),
	}
	sort.Strings(payload.AccessModes)
	body, _ := json.Marshal(payload)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// ValidateBuckets 是启动期强校验：id 唯一、租户 UUID 合法、物理桶只映射一个租户。
func ValidateBuckets(buckets []BucketConfig) error {
	ids := map[string]bool{}
	physical := map[string]string{}
	for _, b := range buckets {
		if !bucketID.MatchString(b.ID) || ids[b.ID] || b.Name == "" {
			return fmt.Errorf("object_storage.managed_buckets: invalid or duplicate id")
		}
		ids[b.ID] = true
		if !CanonicalUUID(b.TenantID) {
			return fmt.Errorf("object_storage.managed_buckets: tenant_id must be a canonical Resource tenant UUID")
		}
		if b.Bucket == "" || len(b.Bucket) > 255 {
			return fmt.Errorf("object_storage.managed_buckets: bucket name required")
		}
		if owner, ok := physical[b.Bucket]; ok && owner != b.TenantID {
			return fmt.Errorf("object_storage.managed_buckets: one physical bucket cannot map to multiple tenants")
		}
		physical[b.Bucket] = b.TenantID
	}
	return nil
}

// ValidateObjectKey 只做传输边界校验；key 原样交给 SDK 编码，不做 path.Clean。
func ValidateObjectKey(key string) error {
	if key == "" || len(key) > 1024 || strings.ContainsRune(key, 0) {
		return ErrInvalid
	}
	return nil
}

func validatePrefix(prefix string) error {
	if len(prefix) > 1024 || strings.ContainsRune(prefix, 0) {
		return ErrInvalid
	}
	return nil
}

// volumeStates 是 state 筛选允许的 PVC phase 取值。
var volumeStates = map[string]bool{"Pending": true, "Bound": true, "Lost": true}

// NormalizeVolumeFilter 校验并规范化卷列表筛选；空串表示不按该字段筛选。
func NormalizeVolumeFilter(filter VolumeFilter) (VolumeFilter, error) {
	name := strings.TrimSpace(filter.Name)
	if filter.Name != "" && name == "" {
		return VolumeFilter{}, ErrInvalid
	}
	if utf8.RuneCountInString(name) > 128 {
		return VolumeFilter{}, ErrInvalid
	}
	id := strings.TrimSpace(filter.ID)
	if filter.ID != "" && id == "" || len(id) > 253 {
		return VolumeFilter{}, ErrInvalid
	}
	if filter.State != "" && !volumeStates[filter.State] {
		return VolumeFilter{}, ErrInvalid
	}
	return VolumeFilter{Name: name, ID: id, State: filter.State}, nil
}

// NormalizeBucketFilter 校验并规范化可见桶列表筛选；空串表示不按该字段筛选。
func NormalizeBucketFilter(filter BucketFilter) (BucketFilter, error) {
	name := strings.TrimSpace(filter.Name)
	if filter.Name != "" && name == "" {
		return BucketFilter{}, ErrInvalid
	}
	if utf8.RuneCountInString(name) > 128 {
		return BucketFilter{}, ErrInvalid
	}
	id := strings.TrimSpace(filter.ID)
	if filter.ID != "" && id == "" || len(id) > 64 {
		return BucketFilter{}, ErrInvalid
	}
	return BucketFilter{Name: name, ID: id}, nil
}

// matchVolumeFilter 判断记录是否满足全部筛选条件（AND）。
func matchVolumeFilter(record *VolumeRecord, filter VolumeFilter) bool {
	if filter.Name != "" && !strings.Contains(strings.ToLower(record.DisplayName), strings.ToLower(filter.Name)) {
		return false
	}
	if filter.ID != "" && filter.ID != record.Name && filter.ID != record.UID {
		return false
	}
	if filter.State != "" && filter.State != record.Phase {
		return false
	}
	return true
}

// matchBucketFilter 判断桶是否满足全部筛选条件（AND）。
func matchBucketFilter(bucket Bucket, filter BucketFilter) bool {
	if filter.Name != "" && !strings.Contains(strings.ToLower(bucket.Name), strings.ToLower(filter.Name)) {
		return false
	}
	if filter.ID != "" && filter.ID != bucket.ID {
		return false
	}
	return true
}
