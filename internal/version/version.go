// Package version holds the build information, set by the release pipeline with -ldflags -X.
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

// Repo is the project URL, which appears in the User-Agent and on the usage page.
const Repo = "https://github.com/openlinkos/registry-mirrors-relay"

// String is the output of the version subcommand.
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

// UserAgent is the default User-Agent sent to upstreams. The client's User-Agent is not forwarded:
// Docker's contains the client's kernel version, operating system and git commit.
func UserAgent() string { return "registry-mirrors-relay/" + Version + " (+" + Repo + ")" }
