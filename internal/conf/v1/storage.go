package conf

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	bizstorage "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
)

// Validate 执行对象存储启动强校验；任何不满足直接启动失败，不进入部分桶可用状态。
func (c *ObjectStorage) Validate() error {
	if c == nil || strings.TrimSpace(c.Endpoint) == "" {
		if c != nil && len(c.ManagedBuckets) > 0 {
			return fmt.Errorf("object_storage.managed_buckets requires endpoint")
		}
		return nil
	}
	if err := validateObjectEndpoint(c.Endpoint); err != nil {
		return fmt.Errorf("object_storage.endpoint: %w", err)
	}
	if c.BrowserEndpoint != "" {
		if err := validateObjectEndpoint(c.BrowserEndpoint); err != nil {
			return fmt.Errorf("object_storage.browser_endpoint: %w", err)
		}
	}
	if strings.TrimSpace(c.Region) == "" || strings.ContainsAny(c.Region, "\r\n\x00") {
		return fmt.Errorf("object_storage.region required")
	}
	if !filepath.IsAbs(c.CredentialsFile) || strings.ContainsAny(c.CredentialsFile, "\r\n\x00") {
		return fmt.Errorf("object_storage.credentials_file must be an absolute private path")
	}
	if c.CaFile != "" && (!filepath.IsAbs(c.CaFile) || strings.ContainsAny(c.CaFile, "\r\n\x00")) {
		return fmt.Errorf("object_storage.ca_file must be an absolute private path")
	}
	buckets := make([]bizstorage.BucketConfig, 0, len(c.ManagedBuckets))
	for _, bucket := range c.ManagedBuckets {
		buckets = append(buckets, bizstorage.BucketConfig{
			ID:       bucket.GetId(),
			TenantID: bucket.GetTenantId(),
			Name:     bucket.GetName(),
			Bucket:   bucket.GetBucket(),
		})
	}
	if err := bizstorage.ValidateBuckets(buckets); err != nil {
		return err
	}
	return nil
}

func validateObjectEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("absolute http(s) address required")
	}
	return nil
}

// Buckets 返回可用的租户桶配置。
func (c *ObjectStorage) Buckets() []bizstorage.BucketConfig {
	if c == nil {
		return nil
	}
	out := make([]bizstorage.BucketConfig, 0, len(c.ManagedBuckets))
	for _, bucket := range c.ManagedBuckets {
		out = append(out, bizstorage.BucketConfig{
			ID:       bucket.GetId(),
			TenantID: bucket.GetTenantId(),
			Name:     bucket.GetName(),
			Bucket:   bucket.GetBucket(),
		})
	}
	return out
}
