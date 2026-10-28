package storage

import (
	"context"
	"time"
)

// BytesPerGB 固定 1 GB = 1,000,000,000 bytes，与 Kubernetes Quantity "20G" 等价。
const BytesPerGB int64 = 1_000_000_000

// 受管标签与注解。label 只放短值；长值或中文放 annotation。
const (
	LabelManagedBy   = "storage.ani.io/managed-by"
	LabelTenantID    = "storage.ani.io/tenant-id"
	LabelCreateToken = "storage.ani.io/create-token"
	AnnotationName   = "storage.ani.io/display-name"
	AnnotationDigest = "storage.ani.io/request-digest"

	ManagedByValue = "ani-resource-service"

	NamespaceManagedBy = "network.ani.io/managed-by"
	NamespaceTenantID  = "network.ani.io/tenant-id"
	NamespaceOwner     = "ani-network-service"
)

// VolumeMode 只接受 Kubernetes 的两个取值。
const (
	ModeFilesystem = "Filesystem"
	ModeBlock      = "Block"
)

// Class 是页面选择 StorageClass 所需的字段子集。
type Class struct {
	Name                 string
	Provisioner          string
	Default              bool
	AllowVolumeExpansion bool
	VolumeBindingMode    string
	ReclaimPolicy        string
}

// Namespace 是只读归属校验后的租户命名空间。
type Namespace struct {
	Name      string
	Deleting  bool
	ManagedBy string
	TenantID  string
}

// VolumeRecord 是 Kubernetes PVC 的事实投影。RequestedQuantity 保留原始 Quantity
// 字符串，RequestedBytes 用于业务比较。
type VolumeRecord struct {
	Name              string
	UID               string
	DisplayName       string
	StorageClassName  string
	VolumeMode        string
	AccessModes       []string
	RequestedQuantity string
	RequestedBytes    int64
	ObservedQuantity  string
	ObservedBytes     int64
	ObservedKnown     bool
	Phase             string
	Deleting          bool
	ResourceVersion   string
	PVName            string
	ClassReclaim      string
	ClassKnown        bool
	Conditions        []Condition
	Labels            map[string]string
	Annotations       map[string]string
}

// CreateSpec 是 data 层写入 PVC 的完整受管参数。
type CreateSpec struct {
	Name              string
	DisplayName       string
	RequestedQuantity string
	RequestedBytes    int64
	StorageClassName  string
	VolumeMode        string
	AccessModes       []string
	Labels            map[string]string
	Annotations       map[string]string
}

type Condition struct {
	Type               string
	Status             string
	Reason             string
	Message            string
	LastTransitionTime time.Time
}

type Event struct {
	Type          string
	Reason        string
	Message       string
	Count         int32
	LastTimestamp time.Time
}

type Consumer struct {
	Kind      string
	Namespace string
	Name      string
	// Claim 是工作负载引用的 PVC 名称；仅用于后端精确过滤，不下发页面。
	Claim string
}

type PVRecord struct {
	Name          string
	ReclaimPolicy string
}

// Volume 是返回给页面列表/详情的稳定投影。
type Volume struct {
	Name                  string
	UID                   string
	DisplayName           string
	StorageClassName      string
	VolumeMode            string
	AccessModes           []string
	RequestedCapacityGB   int64
	ObservedCapacityBytes int64
	ObservedCapacityKnown bool
	ObservedQuantity      string
	Phase                 string
	Deleting              bool
	ReclaimPolicy         string
	ReclaimPolicyKnown    bool
}

type VolumeDetail struct {
	Volume     Volume
	Conditions []Condition
	Events     []Event
	Consumers  []Consumer
}

// DeletionState 区分删除提交、删除中与原对象不存在。
const (
	DeletionSubmitted   = "SUBMITTED"
	DeletionTerminating = "TERMINATING"
	DeletionGone        = "GONE"
)

type DeleteResult struct {
	State  string
	Volume Volume
}

type Bucket struct {
	ID   string
	Name string
}

// VolumeFilter 是卷列表的筛选条件；空串表示不按该字段筛选。
// Name 按 display_name 不区分大小写包含匹配；ID 精确匹配 pvc_name 或 uid；
// State 精确匹配 PVC phase。
type VolumeFilter struct {
	Name  string
	ID    string
	State string
}

// BucketFilter 是可见桶列表的筛选条件；空串表示不按该字段筛选。
// Name 按展示名不区分大小写包含匹配；ID 精确匹配逻辑桶 ID。
type BucketFilter struct {
	Name string
	ID   string
}

// CreateRequest 是服务层传入的原始创建参数；create_token 是业务重试键。
type CreateRequest struct {
	CreateToken      string
	DisplayName      string
	CapacityGB       int64
	StorageClassName string
	VolumeMode       string
	AccessModes      []string
}

type BucketConfig struct {
	ID       string
	TenantID string
	Name     string
	Bucket   string
}

type ObjectItem struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
}

type ObjectPage struct {
	Items          []ObjectItem
	CommonPrefixes []string
	NextToken      string
	IsTruncated    bool
}

type ObjectMeta struct {
	Key          string
	Size         int64
	ETag         string
	ContentType  string
	LastModified time.Time
}

// VolumePort 是 Kubernetes 出站端口，全部方法都按可信 namespace 调用。
type VolumePort interface {
	GetNamespace(ctx context.Context, name string) (Namespace, error)
	GetVolume(ctx context.Context, namespace, name string) (*VolumeRecord, error)
	CreateVolume(ctx context.Context, namespace string, spec CreateSpec) (*VolumeRecord, error)
	ListVolumes(ctx context.Context, namespace, tenantID string) ([]*VolumeRecord, error)
	ResizeVolume(ctx context.Context, namespace, name, expectedUID, expectedResourceVersion, quantity string, capacityBytes int64) (*VolumeRecord, error)
	DeleteVolume(ctx context.Context, namespace, name, uid string) error
	ListStorageClasses(ctx context.Context) ([]Class, error)
	GetStorageClass(ctx context.Context, name string) (*Class, error)
	GetPersistentVolume(ctx context.Context, name string) (*PVRecord, error)
	ListPodConsumers(ctx context.Context, namespace, claimName string) ([]Consumer, error)
	ListNotebookConsumers(ctx context.Context, namespace, claimName string) ([]Consumer, error)
	ListEvents(ctx context.Context, namespace, pvcUID string) ([]Event, error)
}

// ObjectPort 是 S3 出站端口；内部 endpoint 做 List/HEAD，浏览器 endpoint 做预签名。
type ObjectPort interface {
	Available() bool
	ListObjects(ctx context.Context, bucket, prefix, continuationToken string, limit int32) (ObjectPage, error)
	HeadObject(ctx context.Context, bucket, key string) (ObjectMeta, error)
	PresignGetObject(ctx context.Context, bucket, key string, expiry time.Duration) (string, time.Time, error)
}
