package server

import (
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// 容器的内存上限：cgroup v2 和 v1 的文件。
var cgroupFiles = []string{
	"/sys/fs/cgroup/memory.max",                   // v2
	"/sys/fs/cgroup/memory/memory.limit_in_bytes", // v1
}

// readFile 可以在测试里替换。
var readFile = os.ReadFile

// cgroupLimit 返回容器的内存上限（字节）和来源文件；没有上限时返回 0。
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
		// v1 没设上限时是一个接近 2^63 的大数
		if err != nil || n <= 0 || n >= 1<<60 {
			return 0, ""
		}
		return n, f
	}
	return 0, ""
}

// SetMemoryLimit 让 Go 的 GC 在接近容器内存上限前更积极地回收，避免被 OOM 杀掉：
// 有上限时把 GOMEMLIMIT 设为上限的 90%。已经设置了 GOMEMLIMIT 环境变量时不动。返回设置的值（0 表示没设）。
func SetMemoryLimit(log *slog.Logger) int64 {
	if os.Getenv("GOMEMLIMIT") != "" {
		log.Info("使用环境变量 GOMEMLIMIT", "value", os.Getenv("GOMEMLIMIT"))
		return 0
	}
	limit, src := cgroupLimit()
	if limit == 0 {
		return 0
	}
	target := limit / 10 * 9
	debug.SetMemoryLimit(target)
	log.Info("按容器内存上限设置 Go 的内存上限", "cgroup_limit", limit, "gomemlimit", target, "source", src)
	return target
}
