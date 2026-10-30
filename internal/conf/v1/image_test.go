package conf

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"testing"
	"time"
)

func validImageConfig() *Image {
	return &Image{Enabled: true, InstallationId: "11111111-1111-4111-8111-111111111111", DatabaseDsnFile: "/private/dsn", HarborUrl: "https://registry.example.test", HarborCaFile: "/private/ca", HarborUsername: "image-manager", HarborPasswordFile: "/private/password", RobotNamePrefix: "robot$", PlatformProject: "platform-images", EncryptionKeysFile: "/private/keys", CursorSigningKeyFile: "/private/cursor", RequestTimeout: durationpb.New(10 * time.Second), PublisherDays: 30, PullDays: 90}
}
func TestImageConfigurationAdmission(t *testing.T) {
	if err := (*Image)(nil).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (&Image{}).Validate(); err != nil {
		t.Fatal(err)
	}
	c := validImageConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Image){"installation": func(c *Image) { c.InstallationId = "tenant" }, "http": func(c *Image) { c.HarborUrl = "http://registry.test" }, "userinfo": func(c *Image) { c.HarborUrl = "https://user:pass@registry.test" }, "path": func(c *Image) { c.HarborUrl = "https://registry.test/api" }, "missing CA": func(c *Image) { c.HarborCaFile = "" }, "relative secret": func(c *Image) { c.HarborPasswordFile = "password" }, "duration": func(c *Image) { c.PullDays = -1 }, "prefix": func(c *Image) { c.RobotNamePrefix = "" }, "project": func(c *Image) { c.PlatformProject = "../x" }, "timeout": func(c *Image) { c.RequestTimeout = durationpb.New(time.Minute) }} {
		t.Run(name, func(t *testing.T) {
			v := proto.Clone(c).(*Image)
			mutate(v)
			if err := v.Validate(); err == nil {
				t.Fatal("unsafe config accepted")
			}
		})
	}
	c.AllowNeverExpires = true
	c.PullDays = -1
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	base := validConfig()
	base.Image = &Image{}
	if err := base.Validate(); err != nil {
		t.Fatal("disabled Image affected Network", err)
	}
	base.Image = validImageConfig()
	base.Image.HarborUrl = "http://registry.test"
	if err := base.Validate(); err == nil {
		t.Fatal("Bootstrap ignored invalid enabled Image")
	}
}

func TestImagePullSecretWriterConfiguration(t *testing.T) {
	c := validImageConfig()
	// No writer configured keeps Ensure unavailable; that is a valid deployment.
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.PullSecretKubeconfig = "/private/kubeconfig"
	if err := c.Validate(); err == nil {
		t.Fatal("kubeconfig without cluster identity accepted")
	}
	c = validImageConfig()
	c.PullSecretClusterId = "cluster-a"
	c.PullSecretCallers = []*PullSecretCaller{{Subject: "ani-modeldev-service", ClusterIds: []string{"cluster-a"}, Namespaces: []string{"tenant-training"}, OwnerLabels: []string{"ani-modeldev-service"}, RequireTenantLabel: true}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Image){
		"caller cluster mismatch": func(c *Image) { c.PullSecretCallers[0].ClusterIds = []string{"cluster-b"} },
		"duplicate caller": func(c *Image) {
			c.PullSecretCallers = append(c.PullSecretCallers, &PullSecretCaller{Subject: "ani-modeldev-service", ClusterIds: []string{"cluster-a"}, Namespaces: []string{"other"}})
		},
		"empty namespace":        func(c *Image) { c.PullSecretCallers[0].Namespaces = []string{""} },
		"tenant label unlabeled": func(c *Image) { c.PullSecretCallers[0].OwnerLabels = nil },
		"relative kubeconfig":    func(c *Image) { c.PullSecretKubeconfig = "kubeconfig" },
	} {
		t.Run(name, func(t *testing.T) {
			v := proto.Clone(c).(*Image)
			mutate(v)
			if err := v.Validate(); err == nil {
				t.Fatal("unsafe pull secret configuration accepted")
			}
		})
	}
}
