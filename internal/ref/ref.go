// Package ref 校验镜像名、tag 和 digest，语法和 OCI distribution 规范一致。
package ref

import (
	"regexp"
	"strings"
)

var (
	// 仓库名的一段：小写字母数字，中间可以有 .、_、__ 或若干个 -
	componentRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	tagRe       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRe    = regexp.MustCompile(`^(?:sha256:[a-f0-9]{64}|sha512:[a-f0-9]{128})$`)
)

// MaxNameLength 是仓库名（不含 registry）的最大长度。
const MaxNameLength = 255

// ValidName 判断仓库名（如 library/nginx）是否合法。
func ValidName(name string) bool {
	if name == "" || len(name) > MaxNameLength {
		return false
	}
	for _, c := range strings.Split(name, "/") {
		if !componentRe.MatchString(c) {
			return false
		}
	}
	return true
}

// ValidTag 判断 tag 是否合法。
func ValidTag(tag string) bool { return tagRe.MatchString(tag) }

// ValidDigest 判断 digest 是否合法。只接受 sha256 和 sha512。
func ValidDigest(d string) bool { return digestRe.MatchString(d) }

// IsDigest 判断引用看起来是不是 digest（含冒号）。
func IsDigest(r string) bool { return strings.Contains(r, ":") }
