package ref

import (
	"strings"
	"testing"
)

func TestValid(t *testing.T) {
	for name, want := range map[string]bool{
		"library/nginx": true, "a": true, "a/b/c": true, "my_app/x-y.z": true, "a__b": true, "a--b": true,
		"": false, "A": false, "a//b": false, "/a": false, "a/": false, "a_-b": false, ".a": false, "a.": false,
		strings.Repeat("a", 256): false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v", name, got)
		}
	}
	for tag, want := range map[string]bool{"latest": true, "v1.2.3-rc_1": true, "_x": true, ".x": false, "-x": false, "": false, strings.Repeat("t", 129): false} {
		if got := ValidTag(tag); got != want {
			t.Errorf("ValidTag(%q) = %v", tag, got)
		}
	}
	d := "sha256:" + strings.Repeat("a", 64)
	for dg, want := range map[string]bool{d: true, "sha512:" + strings.Repeat("0", 128): true, "sha256:" + strings.Repeat("A", 64): false, "md5:abc": false, "sha256:abc": false} {
		if got := ValidDigest(dg); got != want {
			t.Errorf("ValidDigest(%q) = %v", dg, got)
		}
	}
}
