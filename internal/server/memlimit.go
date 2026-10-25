package server

import (
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Container memory limit: the cgroup v2 and v1 files.
var cgroupFiles = []string{
	"/sys/fs/cgroup/memory.max",                   // v2
	"/sys/fs/cgroup/memory/memory.limit_in_bytes", // v1
}

// readFile can be replaced in tests.
var readFile = os.ReadFile

// cgroupLimit returns the container's memory limit (in bytes) and the file it came from; 0 when there is no
// limit.
func cgroupLimit() (int64, string) {
	for _, f := range cgroupFiles {
		b, err := readFile(f)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(b))
		if s == "max" {
			return 0, ""
		}
		n, err := strconv.ParseInt(s, 10, 64)
		// v1 without a limit reports a large number close to 2^63
		if err != nil || n <= 0 || n >= 1<<60 {
			return 0, ""
		}
		return n, f
	}
	return 0, ""
}

// SetMemoryLimit makes the Go GC collect more aggressively before reaching the container memory limit, to
// avoid being OOM-killed: when there is a limit, GOMEMLIMIT is set to 90% of it. It leaves things alone when
// the GOMEMLIMIT environment variable is already set. It returns the value set (0 means none was set).
func SetMemoryLimit(log *slog.Logger) int64 {
	if os.Getenv("GOMEMLIMIT") != "" {
		log.Info("using GOMEMLIMIT environment variable", "value", os.Getenv("GOMEMLIMIT"))
		return 0
	}
	limit, src := cgroupLimit()
	if limit == 0 {
		return 0
	}
	target := limit / 10 * 9
	debug.SetMemoryLimit(target)
	log.Info("Go memory limit set from container memory limit", "cgroup_limit", limit, "gomemlimit", target, "source", src)
	return target
}
