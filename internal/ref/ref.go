// Package ref validates image names, tags and digests, using the syntax of the OCI distribution
// specification.
package ref

import (
	"regexp"
	"strings"
)

var (
	// One component of a repository name: lowercase letters and digits, optionally separated by
	// ., _, __ or one or more -
	componentRe = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	tagRe       = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRe    = regexp.MustCompile(`^(?:sha256:[a-f0-9]{64}|sha512:[a-f0-9]{128})$`)
)

// MaxNameLength is the maximum length of a repository name (excluding the registry).
const MaxNameLength = 255

// ValidName reports whether a repository name (such as library/nginx) is valid.
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

// ValidTag reports whether a tag is valid.
func ValidTag(tag string) bool { return tagRe.MatchString(tag) }

// ValidDigest reports whether a digest is valid. Only sha256 and sha512 are accepted.
func ValidDigest(d string) bool { return digestRe.MatchString(d) }

// IsDigest reports whether a reference looks like a digest (contains a colon).
func IsDigest(r string) bool { return strings.Contains(r, ":") }
