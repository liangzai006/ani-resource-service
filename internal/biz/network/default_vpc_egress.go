package biz

import (
	"context"
	"strings"
	"time"
)

// DefaultVPCIdentity is a deployment guard. It is never populated from an API
// request or from a tenant VPC lookup.
type DefaultVPCIdentity struct{ ClusterID, ClusterUID, Namespace, Name, UID string }

func (i DefaultVPCIdentity) Validate() error {
	if i.ClusterID == "" || i.Namespace != "kcn-system" || i.Name != "kcn-cluster" {
		return Fail(InvalidArgument, "the configured platform default VPC identity is required")
	}
	for _, s := range []string{i.ClusterID, i.ClusterUID, i.UID} {
		if s == "" || len(s) > 128 || strings.TrimSpace(s) != s || strings.ContainsAny(s, "/\\\t\r\n ") {
			return Fail(InvalidArgument, "the configured cluster and default VPC UIDs are required")
		}
	}
	return nil
}

type DefaultVPCEgress struct {
	EgressMetadata
	PoolID, Phase, OperationState, Address, EIPID                      string
	Identity                                                           DefaultVPCIdentity
	EIPProviderName, EIPProviderUID, SnatProviderName, SnatProviderUID string
	PendingAction                                                      string
	PendingUnknown, DesiredEnabled                                     bool
	AppliedEnabled                                                     *bool
	TargetGeneration                                                   int64
}
type DefaultVPCEgressIntent struct{ Name, PoolID, IdempotencyKey string }
type DefaultEgressPoolProof struct {
	Revision          int64
	ProviderUID, Hash string
	ObservedAt        time.Time
}
type DefaultVPCEgressRepository interface {
	CreateDefaultVPCEgress(context.Context, DefaultVPCEgressIntent, Attribution) (DefaultVPCEgress, error)
	GetDefaultVPCEgress(context.Context, string) (DefaultVPCEgress, error)
	GetCurrentDefaultVPCEgress(context.Context) (DefaultVPCEgress, error)
	DeleteDefaultVPCEgress(context.Context, string, Attribution) (DefaultVPCEgress, error)
}
type DefaultVPCEgressUseCase struct {
	repository    DefaultVPCEgressRepository
	authorization EgressAuthorization
}

func NewDefaultVPCEgress(r DefaultVPCEgressRepository, a EgressAuthorization) (*DefaultVPCEgressUseCase, error) {
	if r == nil || a == nil {
		return nil, Fail(InvalidArgument, "default VPC egress repository and platform authorization are required")
	}
	return &DefaultVPCEgressUseCase{r, a}, nil
}
func (u *DefaultVPCEgressUseCase) Create(ctx context.Context, i DefaultVPCEgressIntent) (DefaultVPCEgress, error) {
	a, err := u.authorization.Platform(ctx)
	if err != nil {
		return DefaultVPCEgress{}, err
	}
	i.Name, err = normalizeEgressName(i.Name, "", i.IdempotencyKey)
	if err != nil {
		return DefaultVPCEgress{}, err
	}
	if !validEgressID(i.PoolID, "pool") {
		return DefaultVPCEgress{}, Fail(InvalidArgument, "a public pool ID is required")
	}
	return u.repository.CreateDefaultVPCEgress(ctx, i, a)
}
func (u *DefaultVPCEgressUseCase) Get(ctx context.Context, id string) (DefaultVPCEgress, error) {
	if _, err := u.authorization.Platform(ctx); err != nil {
		return DefaultVPCEgress{}, err
	}
	if !validEgressID(id, "dve") {
		return DefaultVPCEgress{}, Fail(ResourceNotFound, "platform default VPC egress not found")
	}
	return u.repository.GetDefaultVPCEgress(ctx, id)
}
func (u *DefaultVPCEgressUseCase) Delete(ctx context.Context, id string) (DefaultVPCEgress, error) {
	a, err := u.authorization.Platform(ctx)
	if err != nil {
		return DefaultVPCEgress{}, err
	}
	if !validEgressID(id, "dve") {
		return DefaultVPCEgress{}, Fail(ResourceNotFound, "platform default VPC egress not found")
	}
	return u.repository.DeleteDefaultVPCEgress(ctx, id, a)
}

func (u *DefaultVPCEgressUseCase) GetCurrent(ctx context.Context) (DefaultVPCEgress, error) {
	if _, err := u.authorization.Platform(ctx); err != nil {
		return DefaultVPCEgress{}, err
	}
	return u.repository.GetCurrentDefaultVPCEgress(ctx)
}

type DefaultEgressWork struct {
	Egress                                              DefaultVPCEgress
	Owner, EIPBindingID, SnatBindingID, PoolProviderUID string
	Epoch, PoolRevision                                 int64
	Now                                                 time.Time
}
type DefaultEgressObservation struct {
	EIPExists, SnatExists, EIPReady, Ready, Unbound bool
	EIPUID, SnatUID, Address, Hash                  string
	AppliedEnabled                                  *bool
	TargetGeneration                                int64
	ObservedAt                                      time.Time
}
type DefaultEgressProgress struct {
	State                                  ResourceState
	Phase                                  string
	Reason                                 Reason
	OperationState                         OperationState
	Observation                            DefaultEgressObservation
	Observed, ClearPending, PendingUnknown bool
	Delay                                  time.Duration
}
type DefaultEgressWorkRepository interface {
	ClaimDefaultVPCEgress(context.Context, string, time.Duration) (DefaultEgressWork, bool, error)
	BeginDefaultVPCEgressMutation(context.Context, DefaultEgressWork, string) error
	AssertDefaultVPCEgressLease(context.Context, DefaultEgressWork) error
	FinishDefaultVPCEgress(context.Context, DefaultEgressWork, DefaultEgressProgress) error
}
type DefaultEgressProvider interface {
	ValidateDefaultVPCEgress(context.Context, DefaultVPCIdentity, string) (DefaultEgressPoolProof, error)
	ObserveDefaultVPCEgress(context.Context, DefaultEgressWork) (DefaultEgressObservation, error)
	MutateDefaultVPCEgress(context.Context, DefaultEgressWork, string) error
}
