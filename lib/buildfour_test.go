package lib

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildFourContainers mirrors a real build that adds FOUR containers: the
// main service + a Postgres DB + a Redis + a companion container. It verifies
// all four blocks land, the main service is wired to the DB+Redis (env URLs),
// the per-service network is registered external at the top level, and there
// are no gaps. Cleans up after itself.
func TestBuildFourContainers(t *testing.T) {
	dir := stacksDir()
	stack := "_fourtest"
	fpath := filepath.Join(dir, stack+".yml")
	initial := "name: _fourtest\n\n" +
		"x-common-caps: &common-caps\n  restart: unless-stopped\n\n" +
		"networks:\n  traefik_net: {name: traefik_net, external: true}\n\n" +
		"services:\n"
	if err := os.WriteFile(fpath, []byte(initial), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	defer os.Remove(fpath)

	cfg := buildLoadConf()
	svcNet := "myapp_net"
	db := &buildDBRec{Type: "postgres", Name: "myapp-postgres", IP: "192.168.1.231", Port: "5432",
		Image: "postgres:16-alpine", Password: "changeme", DBName: "myapp", New: true, Net: svcNet, Stack: stack + ".yml"}
	redis := &buildDBRec{Type: "redis", Name: "myapp-redis", IP: "192.168.1.232", Port: "6379",
		Image: "redis:7-alpine", New: true, Net: svcNet, Stack: stack + ".yml"}

	// 1) main service (wired to db + redis)
	main := buildSvc("myapp", "redis:alpine", "192.168.1.230", "8080", cfg, 1, db, redis)
	if !buildInject(stack, main, svcNet, "") {
		t.Fatal("main inject failed")
	}
	// 2) DB  3) Redis
	dblk, dvol := buildDBBlock(db, "myapp")
	buildInject(stack, dblk, svcNet, dvol)
	rblk, rvol := buildDBBlock(redis, "myapp")
	buildInject(stack, rblk, svcNet, rvol)
	// 4) companion
	comp := buildSvc("myapp-worker", "redis:alpine", "192.168.1.233", "8080", cfg, 4, nil, nil)
	buildInject(stack, comp, svcNet, "")

	out, _ := os.ReadFile(fpath)
	s := string(out)

	for _, n := range []string{"myapp", "myapp-postgres", "myapp-redis", "myapp-worker"} {
		if !strings.Contains(s, "  "+n+":") {
			t.Errorf("container %s missing", n)
		}
	}
	if strings.Contains(s, "\n\n\n") {
		t.Errorf("GAP DETECTED: 2+ consecutive blank lines")
	}
	if !strings.Contains(s, "myapp_net: {name: myapp_net, external: true}") {
		t.Errorf("myapp_net not registered external at top level")
	}
	if !strings.Contains(s, "DATABASE_URL=postgresql://") {
		t.Errorf("main service not wired to the postgres DB")
	}
	if !strings.Contains(s, "REDIS_URL=redis://") {
		t.Errorf("main service not wired to redis")
	}
	t.Logf("FOUR-CONTAINER RESULT:\n%s", s)
}
