// Package version 记录构建信息，由发布流水线用 -ldflags -X 写入。
package version

import (
	"fmt"
	"runtime"
)

var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

// Repo 是项目地址，出现在 User-Agent 和用法页里。
const Repo = "https://github.com/openlinkos/registry-mirrors-relay"

// String 是 version 子命令的输出。
func String() string {
	s := "registry-mirrors-relay " + Version
	if Commit != "" {
		s += " (" + Commit
		if Date != "" {
			s += ", " + Date
		}
		s += ")"
	}
	return fmt.Sprintf("%s %s %s/%s", s, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}

// UserAgent 是发往上游的默认 User-Agent。不转发客户端的 User-Agent：Docker 的里面有客户端的内核版本、系统和 git 提交号。
func UserAgent() string { return "registry-mirrors-relay/" + Version + " (+" + Repo + ")" }
