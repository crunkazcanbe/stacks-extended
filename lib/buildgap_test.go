package lib

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildInjectNoGaps adds THREE services to a throwaway stack file (laid out
// like the real ones: anchors → networks: → services:, services running to the
// end) and verifies the result has NO gaps (never 2+ blank lines in a row) and
// every service landed in the services section. Cleans up after itself.
func TestBuildInjectNoGaps(t *testing.T) {
	dir := stacksDir()
	stack := "_gaptest"
	fpath := filepath.Join(dir, stack+".yml")
	initial := "name: _gaptest\n\n" +
		"x-common-caps: &common-caps\n  restart: unless-stopped\n\n" +
		"networks:\n  traefik_net: {name: traefik_net, external: true}\n\n" +
		"services:\n\n" +
		"  existingsvc:\n    image: nginx:alpine\n    container_name: existingsvc\n" +
		"    networks:\n      traefik_net:\n        priority: 1000\n"
	if err := os.WriteFile(fpath, []byte(initial), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	defer os.Remove(fpath)

	cfg := buildLoadConf()
	names := []string{"svcone", "svctwo", "svcthree"}
	for i, n := range names {
		block := buildSvc(n, "redis:alpine", fmt.Sprintf("192.168.1.%d", 221+i), "8080", cfg, i+2, nil, nil)
		if !buildInject(stack, block, n+"_net", "") {
			t.Fatalf("inject %s failed", n)
		}
	}

	out, _ := os.ReadFile(fpath)
	s := string(out)

	if strings.Contains(s, "\n\n\n") {
		t.Errorf("GAP DETECTED: file has 2+ consecutive blank lines")
	}
	for _, n := range names {
		if !strings.Contains(s, "  "+n+":") {
			t.Errorf("%s was not inserted", n)
		}
	}
	// every new network registered at top level
	for _, n := range names {
		if !strings.Contains(s, "  "+n+"_net: {name:") {
			t.Errorf("%s_net not registered top-level", n)
		}
	}
	t.Logf("RESULT FILE:\n%s", s)
}
