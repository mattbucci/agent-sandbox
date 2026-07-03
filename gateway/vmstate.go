package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
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

// ListVMs scans <stateDir>/vms/*/info.json and returns every live VM (has an
// IP and an alive firecracker process), sorted by agent type then instance id.
// Used by the dashboard for liveness and squid ip->agent mapping. Unreadable
// or malformed entries are skipped; a missing state dir yields an empty list.
func ListVMs(stateDir string) []VMInfo {
	pattern := filepath.Join(stateDir, "vms", "*", "info.json")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil
	}
	var vms []VMInfo
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var info VMInfo
		if err := json.Unmarshal(data, &info); err != nil {
			continue
		}
		if info.VMIP == "" || !pidAlive(info.FirecrackerPID) {
			continue
		}
		vms = append(vms, info)
	}
	sort.Slice(vms, func(i, j int) bool {
		if vms[i].AgentType != vms[j].AgentType {
			return vms[i].AgentType < vms[j].AgentType
		}
		return vms[i].InstanceID < vms[j].InstanceID
	})
	return vms
}
