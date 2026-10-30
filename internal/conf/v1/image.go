package conf

import (
	"fmt"
	"github.com/google/uuid"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func (c *Image) Validate() error {
	if c == nil || !c.Enabled {
		return nil
	}
	invalid := func() error { return fmt.Errorf("invalid enabled Image configuration") }
	id, err := uuid.Parse(c.InstallationId)
	if err != nil || id == uuid.Nil || id.String() != c.InstallationId {
		return invalid()
	}
	u, err := url.Parse(c.HarborUrl)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || c.HarborUsername == "" || strings.ContainsAny(c.HarborUsername, ":\r\n") {
		return invalid()
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_$-]{1,64}$`).MatchString(c.RobotNamePrefix) || !regexp.MustCompile(`^[a-z][a-z0-9-]{1,46}[a-z0-9]$`).MatchString(c.PlatformProject) {
		return invalid()
	}
	for _, path := range []string{c.DatabaseDsnFile, c.HarborCaFile, c.HarborPasswordFile, c.EncryptionKeysFile, c.CursorSigningKeyFile} {
		if !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") {
			return invalid()
		}
	}
	if err := validateDuration("Image registry request", c.RequestTimeout, 30*time.Second); err != nil {
		return err
	}
	for _, days := range []int64{c.PublisherDays, c.PullDays} {
		if days == -1 && c.AllowNeverExpires {
			continue
		}
		if days < 1 || days > 3650 {
			return invalid()
		}
	}
	if err := c.validatePullSecretWriter(); err != nil {
		return err
	}
	return nil
}

// validatePullSecretWriter rejects a half-configured unified writer. An empty
// cluster id keeps Ensure unavailable, which is a valid deployment choice; any
// supplied field then requires a usable, explicit authorization table.
func (c *Image) validatePullSecretWriter() error {
	invalid := fmt.Errorf("invalid enabled Image pull secret configuration")
	if c.PullSecretClusterId == "" {
		if c.PullSecretKubeconfig != "" || len(c.PullSecretCallers) != 0 {
			return invalid
		}
		return nil
	}
	if strings.ContainsAny(c.PullSecretClusterId, "\r\n\x00") || (c.PullSecretKubeconfig != "" && (!filepath.IsAbs(c.PullSecretKubeconfig) || strings.ContainsAny(c.PullSecretKubeconfig, "\r\n\x00"))) {
		return invalid
	}
	seen := map[string]bool{}
	for _, caller := range c.PullSecretCallers {
		if caller == nil || caller.Subject == "" || seen[caller.Subject] || len(caller.ClusterIds) == 0 || len(caller.Namespaces) == 0 {
			return invalid
		}
		seen[caller.Subject] = true
		if !containsValue(caller.ClusterIds, c.PullSecretClusterId) {
			return invalid
		}
		for _, namespace := range caller.Namespaces {
			if namespace == "" || strings.ContainsAny(namespace, "\r\n\x00") {
				return invalid
			}
		}
		for _, owner := range caller.OwnerLabels {
			if owner == "" {
				return invalid
			}
		}
		if caller.RequireTenantLabel && len(caller.OwnerLabels) == 0 {
			return invalid
		}
	}
	return nil
}

func containsValue(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
