package main

// docker.go — faithful Go port of stacks_docker.py: the single shared Docker
// access layer for every command. API-FIRST (Engine API via the compiled-in SDK)
// WITH CLI FALLBACK (shell out to `docker`) when STACKS_FORCE_CLI=1 or the API
// can't be reached — so it behaves like the Python on any install.
//
// Mirrors: env, cli, client, api_mode/available, container_state_map,
// container_info, containers, exists, remove_container, start, stop, state,
// inspect, is_running, is_unhealthy, networks, network_table, remove_network,
// running_names, image_inspect, volumes, images.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

var (
	_cli      *client.Client
	_apiOK    *bool
	dockerCtx = context.Background()
)

// dockerEnv mirrors _env(): ensure DOCKER_HOST has a default for the CLI fallback.
func dockerEnv() []string {
	e := os.Environ()
	if os.Getenv("DOCKER_HOST") == "" {
		e = append(e, "DOCKER_HOST=unix:///var/run/docker.sock")
	}
	return e
}

// cliResult is a CLI fallback invocation result (mirrors subprocess.run).
type cliResult struct {
	stdout   string
	stderr   string
	exitCode int
}

// cli mirrors _cli(): run `docker <args>` with a 60s timeout.
func cli(args ...string) cliResult {
	ctx, cancel := context.WithTimeout(dockerCtx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Env = dockerEnv()
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	rc := 0
	if err != nil {
		rc = 1
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		}
	}
	return cliResult{out.String(), errb.String(), rc}
}

// dockerClient mirrors client(): cached client that negotiates the API version.
func dockerClient() *client.Client {
	if _cli == nil {
		c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			panic(err)
		}
		_cli = c
	}
	return _cli
}

// apiMode mirrors api_mode(): true if the Engine API can/should be used.
func apiMode() bool {
	if _apiOK == nil {
		v := true
		if os.Getenv("STACKS_FORCE_CLI") == "1" {
			v = false
		} else {
			ctx, cancel := context.WithTimeout(dockerCtx, 30*time.Second)
			defer cancel()
			if _, err := dockerClient().Ping(ctx); err != nil {
				v = false
			}
		}
		_apiOK = &v
	}
	return *_apiOK
}

func available() bool { return apiMode() }

// ── Containers: data ─────────────────────────────────────────────────────────

// rawContainers mirrors _raw_containers(): one ContainerList(all=true) call.
func rawContainers() []types.Container {
	list, err := dockerClient().ContainerList(dockerCtx, container.ListOptions{All: true})
	if err != nil {
		return nil
	}
	return list
}

// nameOf mirrors _name_of(): first name, leading slash stripped.
func nameOf(c types.Container) string {
	if len(c.Names) == 0 {
		return "?"
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// healthFromStatus mirrors _health_from_status().
func healthFromStatus(status string) string {
	switch {
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	default:
		return ""
	}
}

// containerStateMap mirrors container_state_map(): {name: state}.
func containerStateMap() map[string]string {
	out := map[string]string{}
	if apiMode() {
		for _, d := range rawContainers() {
			out[nameOf(d)] = d.State
		}
		return out
	}
	r := cli("ps", "-a", "--format", "{{.Names}}\t{{.State}}")
	for _, line := range strings.Split(r.stdout, "\n") {
		if n, s, ok := strings.Cut(line, "\t"); ok {
			out[n] = s
		}
	}
	return out
}

// ctrInfo mirrors the per-container dict from container_info().
type ctrInfo struct {
	State   string
	Health  string
	Project string
	Service string
	Image   string
}

// containerInfo mirrors container_info(): {name: {state,health,project,service,image}}.
func containerInfo() map[string]ctrInfo {
	out := map[string]ctrInfo{}
	if apiMode() {
		for _, d := range rawContainers() {
			out[nameOf(d)] = ctrInfo{
				State:   d.State,
				Health:  healthFromStatus(d.Status),
				Project: d.Labels["com.docker.compose.project"],
				Service: d.Labels["com.docker.compose.service"],
				Image:   d.Image,
			}
		}
		return out
	}
	format := "{{.Names}}\t{{.State}}\t{{.Status}}\t{{.Label \"com.docker.compose.project\"}}\t" +
		"{{.Label \"com.docker.compose.service\"}}\t{{.Image}}"
	r := cli("ps", "-a", "--format", format)
	for _, line := range strings.Split(r.stdout, "\n") {
		p := strings.Split(line, "\t")
		if len(p) >= 6 {
			out[p[0]] = ctrInfo{State: p[1], Health: healthFromStatus(p[2]),
				Project: p[3], Service: p[4], Image: p[5]}
		}
	}
	return out
}

// containers mirrors containers(): live container objects (API).
func containers(all bool) []types.Container {
	list, err := dockerClient().ContainerList(dockerCtx, container.ListOptions{All: all})
	if err != nil {
		return nil
	}
	return list
}

// containerExists mirrors exists().
func containerExists(name string) bool {
	if apiMode() {
		_, err := dockerClient().ContainerInspect(dockerCtx, name)
		return err == nil
	}
	_, ok := containerStateMap()[name]
	return ok
}

// ── Containers: actions ──────────────────────────────────────────────────────

// removeContainer mirrors remove_container().
func removeContainer(name string, force, volumes bool) bool {
	if apiMode() {
		err := dockerClient().ContainerRemove(dockerCtx, name,
			container.RemoveOptions{Force: force, RemoveVolumes: volumes})
		return err == nil
	}
	args := []string{"rm"}
	if force {
		args = append(args, "-f")
	}
	if volumes {
		args = append(args, "-v")
	}
	args = append(args, name)
	return cli(args...).exitCode == 0
}

// startContainer mirrors start().
func startContainer(name string) bool {
	if apiMode() {
		return dockerClient().ContainerStart(dockerCtx, name, container.StartOptions{}) == nil
	}
	return cli("start", name).exitCode == 0
}

// stopContainer mirrors stop().
func stopContainer(name string, timeout int) bool {
	if apiMode() {
		return dockerClient().ContainerStop(dockerCtx, name, container.StopOptions{Timeout: &timeout}) == nil
	}
	return cli("stop", "-t", strconv.Itoa(timeout), name).exitCode == 0
}

// containerState mirrors state(): single container's status string or "".
func containerState(name string) string {
	if apiMode() {
		j, err := dockerClient().ContainerInspect(dockerCtx, name)
		if err != nil || j.State == nil {
			return ""
		}
		return j.State.Status
	}
	r := cli("inspect", "-f", "{{.State.Status}}", name)
	if r.exitCode == 0 {
		return strings.TrimSpace(r.stdout)
	}
	return ""
}

// containerInspect mirrors inspect(): full inspect as a generic map (or {} if absent).
func containerInspect(name string) map[string]interface{} {
	empty := map[string]interface{}{}
	if apiMode() {
		_, raw, err := dockerClient().ContainerInspectWithRaw(dockerCtx, name, false)
		if err != nil {
			return empty
		}
		var m map[string]interface{}
		if json.Unmarshal(raw, &m) != nil {
			return empty
		}
		return m
	}
	r := cli("inspect", name)
	if r.exitCode != 0 || strings.TrimSpace(r.stdout) == "" {
		return empty
	}
	var arr []map[string]interface{}
	if json.Unmarshal([]byte(r.stdout), &arr) == nil && len(arr) > 0 {
		return arr[0]
	}
	return empty
}

// isRunning mirrors is_running().
func isRunning(name string) bool { return containerStateMap()[name] == "running" }

// isUnhealthy mirrors is_unhealthy().
func isUnhealthy(name string) bool { return containerInfo()[name].Health == "unhealthy" }

// ── Networks ─────────────────────────────────────────────────────────────────

// netRow mirrors a row of network_table(): (id, name, container_count).
type netRow struct {
	ID    string
	Name  string
	Count int
}

// networks mirrors networks(): all network summaries (API).
func networks() []types.NetworkResource {
	list, err := dockerClient().NetworkList(dockerCtx, types.NetworkListOptions{})
	if err != nil {
		return nil
	}
	return list
}

// networkTable mirrors network_table(): [(id,name,count)], count -1 on inspect failure.
func networkTable() []netRow {
	var rows []netRow
	if apiMode() {
		for _, nd := range networks() {
			cnt := -1
			if ins, err := dockerClient().NetworkInspect(dockerCtx, nd.ID, types.NetworkInspectOptions{}); err == nil {
				cnt = len(ins.Containers)
			}
			rows = append(rows, netRow{nd.ID, nd.Name, cnt})
		}
		return rows
	}
	r := cli("network", "ls", "--format", "{{.ID}}\t{{.Name}}")
	for _, line := range strings.Split(r.stdout, "\n") {
		nid, name, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		cnt := -1
		ins := cli("network", "inspect", nid, "--format", "{{len .Containers}}")
		if v, err := strconv.Atoi(strings.TrimSpace(ins.stdout)); err == nil {
			cnt = v
		}
		rows = append(rows, netRow{nid, name, cnt})
	}
	return rows
}

// removeNetwork mirrors remove_network().
func removeNetwork(netID string) bool {
	if apiMode() {
		return dockerClient().NetworkRemove(dockerCtx, netID) == nil
	}
	return cli("network", "rm", netID).exitCode == 0
}

// runningNames mirrors running_names(): set of running container names.
func runningNames() map[string]bool {
	out := map[string]bool{}
	if apiMode() {
		for _, d := range containers(false) {
			out[nameOf(d)] = true
		}
		return out
	}
	r := cli("ps", "--format", "{{.Names}}")
	for _, ln := range strings.Split(r.stdout, "\n") {
		if ln != "" {
			out[ln] = true
		}
	}
	return out
}

// imageInspect mirrors image_inspect(): raw image inspect as a generic map (or {}).
func imageInspect(img string) map[string]interface{} {
	empty := map[string]interface{}{}
	if apiMode() {
		_, raw, err := dockerClient().ImageInspectWithRaw(dockerCtx, img)
		if err != nil {
			return empty
		}
		var m map[string]interface{}
		if json.Unmarshal(raw, &m) != nil {
			return empty
		}
		return m
	}
	r := cli("inspect", "--type", "image", "--format", "{{json .}}", img)
	if r.exitCode != 0 || strings.TrimSpace(r.stdout) == "" {
		return empty
	}
	var m map[string]interface{}
	if json.Unmarshal([]byte(r.stdout), &m) != nil {
		return empty
	}
	return m
}

// dockerVolumes mirrors volumes(): all volumes (API).
func dockerVolumes() []*volume.Volume {
	resp, err := dockerClient().VolumeList(dockerCtx, volume.ListOptions{})
	if err != nil {
		return nil
	}
	return resp.Volumes
}

// dockerImages mirrors images(): all image summaries (API).
func dockerImages() []image.Summary {
	list, err := dockerClient().ImageList(dockerCtx, image.ListOptions{})
	if err != nil {
		return nil
	}
	return list
}

var _ = network.ListOptions{} // network pkg reserved for future inspect/create helpers
