package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Only reads the current process's existing cgroup. It does not create scopes,
// change quotas, write cgroup files, or request privileges.
func readCgroup() map[string]any {
	if runtime.GOOS != "linux" {
		return nil
	}
	root := "/sys/fs/cgroup"
	if body, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "0::") {
				rel := strings.TrimPrefix(strings.TrimPrefix(line, "0::"), "/")
				clean := filepath.Clean(rel)
				if clean != ".." && !strings.HasPrefix(clean, "../") {
					candidate := filepath.Join(root, clean)
					if _, err := os.Stat(filepath.Join(candidate, "cpu.max")); err == nil {
						root = candidate
					}
				}
			}
		}
	}
	result := map[string]any{"path": root, "scope": "Existing cgroup; may be shared. Limits shown are at this level, not a guarantee of exclusive CPU."}
	read := func(name string) string {
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(body))
	}
	cpuMax := read("cpu.max")
	if cpuMax == "" {
		return nil
	}
	result["cpu_max"] = cpuMax
	fields := strings.Fields(cpuMax)
	if len(fields) == 2 && fields[0] != "max" {
		quota, e1 := strconv.ParseInt(fields[0], 10, 64)
		period, e2 := strconv.ParseInt(fields[1], 10, 64)
		if e1 == nil && e2 == nil && period > 0 {
			result["quota_cores"] = float64(quota) / float64(period)
		}
	}
	result["memory_max"] = read("memory.max")
	for file, key := range map[string]string{"memory.current": "memory_current_bytes", "memory.peak": "memory_peak_bytes"} {
		if value, err := strconv.ParseInt(read(file), 10, 64); err == nil {
			result[key] = value
		}
	}
	for _, line := range strings.Split(read("cpu.stat"), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if value, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			result["cpu_"+fields[0]] = value
		}
	}
	return result
}
func int64Value(m map[string]any, key string) int64 {
	if v, ok := m[key].(int64); ok {
		return v
	}
	return 0
}
func cgroupDelta(before, after map[string]any, peak int64) map[string]any {
	if after == nil {
		return nil
	}
	result := map[string]any{"final": after, "initial": before, "sampled_peak_memory_current_bytes": peak}
	for _, key := range []string{"cpu_usage_usec", "cpu_user_usec", "cpu_system_usec", "cpu_nr_periods", "cpu_nr_throttled", "cpu_throttled_usec"} {
		_, a := after[key]
		_, b := before[key]
		if a && b {
			result[key+"_delta"] = int64Value(after, key) - int64Value(before, key)
		}
	}
	return result
}
