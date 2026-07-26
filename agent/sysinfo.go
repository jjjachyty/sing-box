package agent

import (
	"os"
	"strconv"
	"strings"
	"sync"
)

var lastCPUSample struct {
	sync.Mutex
	idle  uint64
	total uint64
}

// getCPUUsage reads CPU utilization from /proc/stat (Linux), computed against
// the previous sample. Returns 0 on non-Linux or first call.
func getCPUUsage() float64 {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 9 || fields[0] != "cpu" {
		return 0
	}
	var vals [8]uint64
	for i := 0; i < 8; i++ {
		v, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return 0
		}
		vals[i] = v
	}
	idle := vals[3] + vals[4] // idle + iowait
	var total uint64
	for _, v := range vals {
		total += v
	}
	lastCPUSample.Lock()
	defer lastCPUSample.Unlock()
	dTotal := total - lastCPUSample.total
	dIdle := idle - lastCPUSample.idle
	lastCPUSample.total = total
	lastCPUSample.idle = idle
	if dTotal == 0 || dIdle > dTotal {
		return 0
	}
	return float64(dTotal-dIdle) / float64(dTotal) * 100
}

// getMemoryUsage reads memory utilization from /proc/meminfo (Linux).
// Returns 0 on non-Linux.
func getMemoryUsage() float64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	var total, available uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = v
		case "MemAvailable:":
			available = v
		}
	}
	if total == 0 || available > total {
		return 0
	}
	return float64(total-available) / float64(total) * 100
}
