// Package storage adapts the internal Storage gRPC contract to use cases.
package storage

import (
	"context"
	"errors"
	"time"

	storagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/storage/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type Service struct {
	storagev1.UnimplementedStorageServiceServer
	uc *biz.UseCase
}

func NewService(uc *biz.UseCase) (*Service, error) {
	if uc == nil {
		return nil, biz.ErrInvalid
	}
	return &Service{uc: uc}, nil
}

func rpcError(e error) error {
	if e == nil {
		return nil
	}
	switch {
	case errors.Is(e, biz.ErrInvalid):
		return status.Error(codes.InvalidArgument, "INVALID_STORAGE_REQUEST")
	case errors.Is(e, biz.ErrScopeDenied):
		return status.Error(codes.PermissionDenied, "STORAGE_SCOPE_DENIED")
	case errors.Is(e, biz.ErrNotFound):
		return status.Error(codes.NotFound, "STORAGE_RESOURCE_NOT_FOUND")
	case errors.Is(e, biz.ErrAccessDenied):
		return status.Error(codes.PermissionDenied, "STORAGE_ACCESS_DENIED")
	case errors.Is(e, biz.ErrCreateConflict):
		return status.Error(codes.AlreadyExists, "CREATE_TOKEN_CONFLICT")
	case errors.Is(e, biz.ErrIdentityChanged):
		return status.Error(codes.AlreadyExists, "VOLUME_IDENTITY_CHANGED")
	case errors.Is(e, biz.ErrCapacityDecrease):
		return status.Error(codes.InvalidArgument, "CAPACITY_MUST_NOT_DECREASE")
	case errors.Is(e, biz.ErrVolumeInUse):
		return status.Error(codes.AlreadyExists, "VOLUME_IN_USE")
	case errors.Is(e, biz.ErrUsageUnknown):
		return status.Error(codes.Unavailable, "VOLUME_USAGE_UNKNOWN")
	case errors.Is(e, biz.ErrExpansionDenied):
		return status.Error(codes.FailedPrecondition, "VOLUME_EXPANSION_UNSUPPORTED")
	case errors.Is(e, biz.ErrConflict):
		return status.Error(codes.Aborted, "STORAGE_RESOURCE_CONFLICT")
	case errors.Is(e, biz.ErrVolumeTerminating):
		return status.Error(codes.FailedPrecondition, "VOLUME_TERMINATING")
	case errors.Is(e, biz.ErrDownloadUnavailable):
		return status.Error(codes.FailedPrecondition, "DOWNLOAD_ENDPOINT_UNAVAILABLE")
	case errors.Is(e, biz.ErrBackendUnavailable):
		return status.Error(codes.Unavailable, "STORAGE_BACKEND_UNAVAILABLE")
	default:
		return status.Error(codes.Internal, "STORAGE_INTERNAL_ERROR")
	}
}

func (s *Service) ListStorageClasses(ctx context.Context, r *storagev1.ListStorageClassesRequest) (*storagev1.ListStorageClassesResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	items, err := s.uc.ListStorageClasses(ctx, r.GetTenantId())
	if err != nil {
		return nil, rpcError(err)
	}
	response := &storagev1.ListStorageClassesResponse{Items: make([]*storagev1.StorageClass, 0, len(items))}
	for _, item := range items {
		response.Items = append(response.Items, &storagev1.StorageClass{
			Name:                 item.Name,
			Provisioner:          item.Provisioner,
			IsDefault:            item.Default,
			AllowVolumeExpansion: item.AllowVolumeExpansion,
			VolumeBindingMode:    item.VolumeBindingMode,
			ReclaimPolicy:        item.ReclaimPolicy,
		})
	}
	return response, nil
}

func (s *Service) CreateVolume(ctx context.Context, r *storagev1.CreateVolumeRequest) (*storagev1.CreateVolumeResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	volume, err := s.uc.CreateVolume(ctx, r.GetTenantId(), biz.CreateRequest{
		CreateToken:      r.GetCreateToken(),
		DisplayName:      r.GetDisplayName(),
		CapacityGB:       r.GetCapacityGb(),
		StorageClassName: r.GetStorageClassName(),
		VolumeMode:       r.GetVolumeMode(),
		AccessModes:      r.GetAccessModes(),
	})
	if err != nil {
		return nil, rpcError(err)
	}
	return &storagev1.CreateVolumeResponse{Volume: wireVolume(volume)}, nil
}

func (s *Service) ListVolumes(ctx context.Context, r *storagev1.ListVolumesRequest) (*storagev1.ListVolumesResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	items, total, err := s.uc.ListVolumes(ctx, r.GetTenantId(), r.GetPage(), r.GetPageSize(), biz.VolumeFilter{
		Name: r.GetName(), ID: r.GetId(), State: r.GetState(),
	})
	if err != nil {
		return nil, rpcError(err)
	}
	response := &storagev1.ListVolumesResponse{Total: total, Items: make([]*storagev1.Volume, 0, len(items))}
	for _, item := range items {
		response.Items = append(response.Items, wireVolume(item))
	}
	return response, nil
}

func (s *Service) GetVolume(ctx context.Context, r *storagev1.GetVolumeRequest) (*storagev1.GetVolumeResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	detail, err := s.uc.GetVolume(ctx, r.GetTenantId(), r.GetPvcName())
	if err != nil {
		return nil, rpcError(err)
	}
	response := &storagev1.GetVolumeResponse{Detail: &storagev1.VolumeDetail{Volume: wireVolume(detail.Volume)}}
	for _, condition := range detail.Conditions {
		response.Detail.Conditions = append(response.Detail.Conditions, &storagev1.VolumeCondition{
			Type: condition.Type, Status: condition.Status, Reason: condition.Reason, Message: condition.Message,
			LastTransitionTime: timestamp(condition.LastTransitionTime),
		})
	}
	for _, event := range detail.Events {
		response.Detail.Events = append(response.Detail.Events, &storagev1.VolumeEvent{
			Type: event.Type, Reason: event.Reason, Message: event.Message, Count: event.Count,
			LastTimestamp: timestamp(event.LastTimestamp),
		})
	}
	for _, consumer := range detail.Consumers {
		response.Detail.Consumers = append(response.Detail.Consumers, &storagev1.VolumeConsumer{
			Kind: consumer.Kind, Namespace: consumer.Namespace, Name: consumer.Name,
		})
	}
	return response, nil
}

func (s *Service) UpdateVolume(ctx context.Context, r *storagev1.UpdateVolumeRequest) (*storagev1.UpdateVolumeResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	volume, err := s.uc.UpdateVolume(ctx, r.GetTenantId(), r.GetPvcName(), r.GetExpectedUid(), r.GetCapacityGb())
	if err != nil {
		return nil, rpcError(err)
	}
	return &storagev1.UpdateVolumeResponse{Volume: wireVolume(volume)}, nil
}

func (s *Service) DeleteVolume(ctx context.Context, r *storagev1.DeleteVolumeRequest) (*storagev1.DeleteVolumeResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	result, err := s.uc.DeleteVolume(ctx, r.GetTenantId(), r.GetPvcName(), r.GetExpectedUid())
	if err != nil {
		return nil, rpcError(err)
	}
	response := &storagev1.DeleteVolumeResponse{State: result.State}
	if result.Volume.UID != "" {
		response.Volume = wireVolume(result.Volume)
	}
	return response, nil
}

func (s *Service) ListBuckets(ctx context.Context, r *storagev1.ListBucketsRequest) (*storagev1.ListBucketsResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	items, err := s.uc.ListBuckets(r.GetTenantId(), biz.BucketFilter{Name: r.GetName(), ID: r.GetId()})
	if err != nil {
		return nil, rpcError(err)
	}
	response := &storagev1.ListBucketsResponse{Items: make([]*storagev1.Bucket, 0, len(items))}
	for _, item := range items {
		response.Items = append(response.Items, &storagev1.Bucket{Id: item.ID, Name: item.Name})
	}
	return response, nil
}

func (s *Service) ListObjects(ctx context.Context, r *storagev1.ListObjectsRequest) (*storagev1.ListObjectsResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	limitSet := r.Limit != nil
	page, err := s.uc.ListObjects(ctx, r.GetTenantId(), r.GetBucketId(), r.GetPrefix(), limitSet, int(r.GetLimit()), r.GetCursor())
	if err != nil {
		return nil, rpcError(err)
	}
	response := &storagev1.ListObjectsResponse{
		CommonPrefixes: page.CommonPrefixes,
		NextCursor:     page.NextToken,
		IsTruncated:    page.IsTruncated,
		Items:          make([]*storagev1.ObjectItem, 0, len(page.Items)),
	}
	for _, item := range page.Items {
		response.Items = append(response.Items, &storagev1.ObjectItem{
			Key: item.Key, Size: item.Size, Etag: item.ETag, LastModified: timestamp(item.LastModified),
		})
	}
	return response, nil
}

func (s *Service) HeadObject(ctx context.Context, r *storagev1.HeadObjectRequest) (*storagev1.HeadObjectResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	meta, err := s.uc.HeadObject(ctx, r.GetTenantId(), r.GetBucketId(), r.GetKey())
	if err != nil {
		return nil, rpcError(err)
	}
	return &storagev1.HeadObjectResponse{Object: &storagev1.ObjectMeta{
		Key: meta.Key, Size: meta.Size, Etag: meta.ETag, ContentType: meta.ContentType, LastModified: timestamp(meta.LastModified),
	}}, nil
}

func (s *Service) GetDownloadURL(ctx context.Context, r *storagev1.GetDownloadURLRequest) (*storagev1.GetDownloadURLResponse, error) {
	if r == nil {
		return nil, rpcError(biz.ErrInvalid)
	}
	url, expires, err := s.uc.DownloadURL(ctx, r.GetTenantId(), r.GetBucketId(), r.GetKey())
	if err != nil {
		return nil, rpcError(err)
	}
	return &storagev1.GetDownloadURLResponse{Url: url, ExpiresAt: timestamp(expires)}, nil
}

func wireVolume(volume biz.Volume) *storagev1.Volume {
	return &storagev1.Volume{
		PvcName:                  volume.Name,
		Uid:                      volume.UID,
		DisplayName:              volume.DisplayName,
		StorageClassName:         volume.StorageClassName,
		VolumeMode:               volume.VolumeMode,
		AccessModes:              volume.AccessModes,
		RequestedCapacityGb:      volume.RequestedCapacityGB,
		ObservedCapacityBytes:    volume.ObservedCapacityBytes,
		ObservedCapacityKnown:    volume.ObservedCapacityKnown,
		ObservedCapacityQuantity: volume.ObservedQuantity,
		Phase:                    volume.Phase,
		Deleting:                 volume.Deleting,
		ReclaimPolicy:            volume.ReclaimPolicy,
		ReclaimPolicyKnown:       volume.ReclaimPolicyKnown,
	}
}

func timestamp(value time.Time) *timestamppb.Timestamp {
	if value.IsZero() {
		return nil
	}
	return timestamppb.New(value)
}
