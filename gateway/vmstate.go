package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
)

// VMInfo mirrors the relevant fields of state/vms/<id>/info.json written by the
// host tooling. Only the fields the router needs are declared.
type VMInfo struct {
	InstanceID     string `json:"instance_id"`
	AgentType      string `json:"agent_type"`
	Slot           int    `json:"slot"`
	VMIP           string `json:"vm_ip"`
	FirecrackerPID int    `json:"firecracker_pid"`
}

// pidAlive reports whether the process with the given pid is alive, using the
// kill(pid, 0) probe (no signal delivered).
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// findVMForAgent scans <stateDir>/vms/*/info.json and returns the first VM whose
// agent_type matches agent and whose firecracker process is alive. ok is false
// when no such VM exists.
func findVMForAgent(stateDir, agent string) (VMInfo, bool) {
	pattern := filepath.Join(stateDir, "vms", "*", "info.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return VMInfo{}, false
	}
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var info VMInfo
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		if info.AgentType != agent {
			continue
		}
		if info.VMIP == "" || !pidAlive(info.FirecrackerPID) {
			continue
		}
		return info, true
	}
	return VMInfo{}, false
}
