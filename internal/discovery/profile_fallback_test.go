package discovery

import (
	"strings"
	"testing"
)

func TestUnknownProfileIsVisible(t *testing.T) {
	cfg := string(EPPConfig(EPPOptions{EndpointsPath: "/x", Profile: "bogus"}))
	if !strings.Contains(cfg, `profile "bogus" is not one ModelFabric offers`) {
		t.Errorf("the fallback is invisible:\n%s", cfg[:200])
	}
	clean := string(EPPConfig(EPPOptions{EndpointsPath: "/x", Profile: ProfileTuned}))
	if strings.Contains(clean, "is not one ModelFabric offers") {
		t.Error("a known profile was reported as unknown")
	}
}
