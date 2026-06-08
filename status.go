package main

// status.go — `stacks status`: a quick count of containers via the Docker API.

import (
	"fmt"
	"strings"
)

func cmdStatus(args []string) {
	banner()
	list := containers(true)
	running, unhealthy := 0, 0
	for _, c := range list {
		if c.State == "running" {
			running++
		}
		if strings.Contains(c.Status, "(unhealthy)") {
			unhealthy++
		}
	}
	fmt.Printf("\n📊 containers: %d total · %d running · %d stopped · %d unhealthy\n",
		len(list), running, len(list)-running, unhealthy)
}
