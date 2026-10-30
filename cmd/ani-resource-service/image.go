package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	imagedata "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/image"
	imageservice "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/image"
)

type imageComponents struct {
	tenant     *imageservice.TenantService
	lifecycle  *imagebiz.Lifecycle
	catalog    *imagebiz.Catalog
	repository *imagedata.Postgres
	harbor     *imagedata.Harbor
	runtime    *imagebiz.Runtime
}

func (c *imageComponents) Close() {
	if c == nil {
		return
	}
	c.harbor.Close()
	c.repository.Close()
}

// Private files are bounded, regular, not symlinks, and inaccessible to other
// users. Check the opened inode as well, so a path replacement is rejected.
func readImagePrivateFile(path string) ([]byte, error) {
	fail := func() ([]byte, error) {
		return nil, fmt.Errorf("Image private configuration file unavailable or unsafe")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() > 65536 {
		return fail()
	}
	f, err := os.Open(path)
	if err != nil {
		return fail()
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() || after.Mode().Perm()&0077 != 0 {
		return fail()
	}
	body, err := io.ReadAll(io.LimitReader(f, 65537))
	if err != nil || len(body) == 0 || len(body) > 65536 {
		clear(body)
		return fail()
	}
	return body, nil
}

func openImage(ctx context.Context, c *conf.Image) (*imageComponents, error) {
	if c == nil || !c.Enabled {
		return nil, nil
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	dsn, err := readImagePrivateFile(c.DatabaseDsnFile)
	if err != nil {
		return nil, err
	}
	defer clear(dsn)
	password, err := readImagePrivateFile(c.HarborPasswordFile)
	if err != nil {
		return nil, err
	}
	defer clear(password)
	keyBody, err := readImagePrivateFile(c.EncryptionKeysFile)
	if err != nil {
		return nil, err
	}
	defer clear(keyBody)
	cursorBody, err := readImagePrivateFile(c.CursorSigningKeyFile)
	if err != nil {
		return nil, err
	}
	defer clear(cursorBody)
	ca, err := os.ReadFile(c.HarborCaFile)
	if err != nil || len(ca) > 65536 {
		return nil, fmt.Errorf("Image registry CA unavailable")
	}
	var keyConfig struct {
		ActiveKeyID string            `json:"active_key_id"`
		Keys        map[string][]byte `json:"keys"`
	}
	decoder := json.NewDecoder(bytes.NewReader(keyBody))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&keyConfig); err != nil {
		return nil, fmt.Errorf("invalid Image keyring file")
	}
	defer func() {
		for _, key := range keyConfig.Keys {
			clear(key)
		}
	}()
	if decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("invalid Image keyring file")
	}
	cipher, err := imagedata.NewAESGCMKeyring(keyConfig.ActiveKeyID, keyConfig.Keys)
	if err != nil {
		return nil, err
	}
	cursorKey, err := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(string(cursorBody)))
	if err != nil {
		return nil, fmt.Errorf("invalid Image cursor key file")
	}
	defer clear(cursorKey)
	cursor, err := imagebiz.NewCursorCodec(cursorKey)
	if err != nil {
		return nil, err
	}
	harbor, err := imagedata.NewHarbor(imagedata.HarborConfig{URL: c.HarborUrl, Username: c.HarborUsername, Password: password, CAPEM: ca, RobotNamePrefix: c.RobotNamePrefix, Timeout: c.RequestTimeout.AsDuration()})
	if err != nil {
		return nil, err
	}
	ready := false
	defer func() {
		if !ready {
			harbor.Close()
		}
	}()
	// Database readiness is independent of Harbor availability. No provider
	// request occurs during composition; disabled Image opens nothing.
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	repo, err := imagedata.OpenPostgres(dbCtx, strings.TrimSpace(string(dsn)))
	if err != nil {
		return nil, err
	}
	defer func() {
		if !ready {
			repo.Close()
		}
	}()
	u, _ := url.Parse(c.HarborUrl)
	lifecycle, err := imagebiz.NewLifecycle(repo, harbor, cipher, imagebiz.LifecycleConfig{InstallationID: c.InstallationId, RegistryAuthority: u.Host, PlatformProject: c.PlatformProject, PublisherDays: c.PublisherDays, PullDays: c.PullDays, AllowNeverExpires: c.AllowNeverExpires}, time.Now)
	if err != nil {
		return nil, err
	}
	catalog, err := imagebiz.NewCatalog(repo, repo, repo, harbor, cursor)
	if err != nil {
		return nil, err
	}
	runtime, err := imagebiz.NewRuntime(repo, repo, repo, harbor, cipher, time.Now)
	if err != nil {
		return nil, err
	}
	if c.PullSecretClusterId != "" {
		writer, err := imagedata.NewPullSecrets(c.PullSecretClusterId, c.PullSecretKubeconfig)
		if err != nil {
			return nil, err
		}
		callers := make([]imagebiz.PullSecretCaller, 0, len(c.PullSecretCallers))
		for _, caller := range c.PullSecretCallers {
			callers = append(callers, imagebiz.PullSecretCaller{Subject: caller.GetSubject(), ClusterIDs: caller.GetClusterIds(), Namespaces: caller.GetNamespaces(), OwnerLabels: caller.GetOwnerLabels(), RequireTenantLabel: caller.GetRequireTenantLabel()})
		}
		// Instance composition writes through the same use case; only the
		// runtime RPC callers need an explicit deployment allowlist entry.
		if err = runtime.UsePullSecretWriter(writer, writer, callers); err != nil {
			return nil, err
		}
	}
	ready = true
	return &imageComponents{tenant: imageservice.NewTenantService(lifecycle, catalog), lifecycle: lifecycle, catalog: catalog, repository: repo, harbor: harbor, runtime: runtime}, nil
}
