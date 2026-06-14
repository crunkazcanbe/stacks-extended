package lib

import (
	"strings"
	"testing"
)

// TestBuildAutoThreeContainers proves the build auto-fills each new service from
// the user's build.yaml config: networks (traefik_net + own net), bind volume,
// traefik/sablier labels, mac/hostname, and the config-driven blocks
// (common-caps / blkio / ulimits / deploy). Builds THREE services and checks
// each one is fully decorated, then logs the generated YAML so it's visible.
func TestBuildAutoThreeContainers(t *testing.T) {
	cfg := buildLoadConf()
	type svc struct{ name, ip string }
	svcs := []svc{
		{"alpha", "192.168.1.211"},
		{"bravo", "192.168.1.212"},
		{"charlie", "192.168.1.213"},
	}
	// redis:alpine is normally cached locally, so the healthcheck image-probe is fast.
	for i, s := range svcs {
		out := buildSvc(s.name, "redis:alpine", s.ip, "8080", cfg, i+1, nil, nil)

		must := []string{
			"  " + s.name + ":",                 // service key
			"container_name: " + s.name,         // identity
			"hostname: " + s.name,               // auto hostname
			"mac_address:",                      // auto mac
			"networks:",                         // auto networks block
			"traefik_net:",                      // master net
			s.name + "_net:",                    // own container net
			"volumes:",                          // auto volume block
			"/docker/" + s.name + ":/data",      // auto bind mount
			"labels:",                           // auto labels
			"traefik.enable=true",               // traefik label
			"sablier.enable=true",               // sablier label
		}
		for _, m := range must {
			if !strings.Contains(out, m) {
				t.Errorf("[%s] auto-fill MISSING %q", s.name, m)
			}
		}
		// config-driven blocks (these are all ON in build.yaml)
		if cfg.UseCommonCaps && !strings.Contains(out, "<<: *common-caps") {
			t.Errorf("[%s] use_common_caps on but '<<: *common-caps' missing", s.name)
		}
		if cfg.Blkio && !strings.Contains(out, "blkio_config:") {
			t.Errorf("[%s] blkio on but blkio_config missing", s.name)
		}
		if cfg.Ulimits && !strings.Contains(out, "ulimits:") {
			t.Errorf("[%s] ulimits on but ulimits missing", s.name)
		}
		if cfg.DeployLimits && !strings.Contains(out, "deploy:") {
			t.Errorf("[%s] deploy_limits on but deploy missing", s.name)
		}
		t.Logf("\n----- generated service: %s -----%s", s.name, out)
	}
}
