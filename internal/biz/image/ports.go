package biz

import (
	"context"
	"time"
)

// RuntimeImages is consumed by the actual workload owner; Image owns no Pods.
type RuntimeImages interface {
	ResolveImageForWorkload(context.Context, ResolveImage) (ResolvedImage, error)
	GetTenantPullMaterial(context.Context, string) (PullMaterial, error)
}
type SecretCipher interface {
	Seal(context.Context, SecretAAD, Secret) (EncryptedSecret, error)
	Open(context.Context, SecretAAD, EncryptedSecret) (Secret, error)
}
type SpaceRepository interface {
	FindTenantSpace(context.Context, string) (Space, error)
	FindPlatformSpace(context.Context) (Space, error)
	ReserveTenantSpace(context.Context, Space, Command) (Space, Command, error)
}
type CatalogRepository interface {
	FindTenantRegistration(context.Context, string, string) (Registration, error)
	FindPlatformRegistration(context.Context, string) (Registration, error)
	PageTenantRegistrations(context.Context, string, Filter, *PageKey) ([]Registration, int64, error)
	PagePlatformRegistrations(context.Context, Filter, *PageKey) ([]Registration, int64, error)
	FindTenantRegistrationsByReference(context.Context, string, ReferenceLookup) ([]Registration, error)
	FindPlatformRegistrationsByReference(context.Context, ReferenceLookup) ([]Registration, error)
	ApplyTenantRegistration(context.Context, Command, Registration) (Registration, error)
	ApplyTenantMetadata(context.Context, Command, UpdateImage) (Registration, error)
	ApplyTenantUnregister(context.Context, Command, UnregisterImage) (Registration, error)
}

// ReferenceLookup identifies an exact repository and tag or immutable digest
// in one already authorized image space.
type ReferenceLookup struct {
	SpaceID, Repository, Reference string
	IsDigest                       bool
}
type CommandRepository interface {
	FindTenantCommand(context.Context, string, string, string) (Command, error)
	WithSpaceWriteLock(context.Context, string, func(context.Context) error) error
}
type StoredCredential struct {
	Info   CredentialInfo
	Secret EncryptedSecret
	AAD    SecretAAD
}
type CredentialRepository interface {
	GetTenantPublisher(context.Context, string, string) (CredentialInfo, error)
	GetTenantPull(context.Context, string, string) (StoredCredential, error)
}
type Project struct {
	ID      int64
	Name    string
	Private bool
}
type RobotPermission struct{ Project, Resource, Action string }
type Robot struct {
	ID                          int64
	Name, Description, Username string
	Disabled                    bool
	Permissions                 []RobotPermission
	ExpiresAt                   int64
	DurationDays                int64
}
type RobotRequest struct {
	Name, Description string
	DurationDays      int64
	Permissions       []RobotPermission
}
type Artifact struct {
	Digest, MediaType string
	Platforms         []ImagePlatform
}
type Registry interface {
	GetProjectByID(context.Context, int64) (Project, error)
	FindProjectByName(context.Context, string) (Project, error)
	CreatePrivateProject(context.Context, string) (Project, error)
	CreateRobot(context.Context, RobotRequest) (Robot, error)
	FindOwnedRobot(context.Context, RobotRequest) (Robot, error)
	GetRobot(context.Context, int64) (Robot, error)
	SetRobotSecret(context.Context, Robot, Secret) error
	SetRobotDisabled(context.Context, Robot, bool) error
	ResolveArtifact(context.Context, string, string, string) (Artifact, error)
	GetArtifactByDigest(context.Context, string, string, string) (Artifact, error)
}

// LifecycleRepository persists only this domain's bounded external commands.
// All mutations retain explicit tenant scope and CAS in their implementations.
type LifecycleRepository interface {
	SpaceRepository
	CommandRepository
	CredentialRepository
	BindTenantProject(context.Context, Space, int64) (Space, error)
	BeginTenantCredentialCommand(context.Context, Command, int64) (Command, error)
	PrepareTenantCandidate(context.Context, Command) (Command, error)
	SaveTenantCommandPhase(context.Context, Command) (Command, error)
	CompleteTenantCommand(context.Context, Command) (Command, error)
	CompleteTenantEnable(context.Context, Space, Command) (Space, Command, error)
	ActivateTenantCandidate(context.Context, Space, Command, EncryptedSecret) (Space, Command, error)
	CompleteTenantDisable(context.Context, Command) (Command, error)
	BlockTenantSpace(context.Context, Space, Reason) (Space, error)
	InspectSpaceForOperator(context.Context, string) (Space, error)
	OpenTenantExternalCommand(context.Context, string, string) (Command, error)
	RecoverTenantProject(context.Context, Space, Command, int64, string) (Space, error)
	PurgeExpiredDeliverySecrets(context.Context, time.Time) (int64, error)
}

// PlatformRepository has explicit platform-only queries. Only the local
// operator use case receives this port; no tenant input selects these writes.
type PlatformRepository interface {
	FindPlatformSpace(context.Context) (Space, error)
	FindPlatformCommand(context.Context, string, string) (Command, error)
	ReservePlatformSpace(context.Context, Space, Command) (Space, Command, error)
	BindPlatformProject(context.Context, Space, int64) (Space, error)
	BlockPlatformSpace(context.Context, Space, Reason) (Space, error)
	SavePlatformCommandPhase(context.Context, Command) (Command, error)
	CompletePlatformEnable(context.Context, Space, Command) (Space, Command, error)
	GetPlatformPublisher(context.Context, string) (CredentialInfo, error)
	BeginPlatformCredentialCommand(context.Context, Command, int64) (Command, error)
	PreparePlatformCandidate(context.Context, Command) (Command, error)
	ActivatePlatformCandidate(context.Context, Space, Command) (Space, Command, error)
	OpenPlatformExternalCommand(context.Context, string) (Command, error)
	RecoverPlatformProject(context.Context, Space, Command, int64, string) (Space, error)
	ApplyPlatformRegistration(context.Context, Command, Registration) (Registration, error)
	ApplyPlatformMetadata(context.Context, Command, UpdateImage) (Registration, error)
	ApplyPlatformUnregister(context.Context, Command, UnregisterImage) (Registration, error)
}
