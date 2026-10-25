package server

import (
	"errors"
	"log/slog"
	"math"
	"os"
	"runtime/debug"
	"testing"
)

func TestCgroupLimit(t *testing.T) {
	defer func() { readFile = os.ReadFile }()
	cases := []struct {
		files map[string]string
		want  int64
	}{
		{map[string]string{"/sys/fs/cgroup/memory.max": "536870912\n"}, 536870912},
		{map[string]string{"/sys/fs/cgroup/memory.max": "max\n"}, 0},
		{map[string]string{"/sys/fs/cgroup/memory/memory.limit_in_bytes": "268435456\n"}, 268435456},
		{map[string]string{"/sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n"}, 0},
		{map[string]string{}, 0},
	}
	for _, c := range cases {
		files := c.files
		readFile = func(name string) ([]byte, error) {
			if v, ok := files[name]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("no such file")
		}
		if got, _ := cgroupLimit(); got != c.want {
			t.Errorf("%v: want %d, got %d", c.files, c.want, got)
		}
	}
}

func TestSetMemoryLimit(t *testing.T) {
	defer func() { readFile = os.ReadFile }()
	defer debug.SetMemoryLimit(math.MaxInt64)
	readFile = func(name string) ([]byte, error) {
		if name == "/sys/fs/cgroup/memory.max" {
			return []byte("1000000000"), nil
		}
		return nil, errors.New("no")
	}
	t.Setenv("GOMEMLIMIT", "")
	if got := SetMemoryLimit(slog.New(slog.DiscardHandler)); got != 900000000 {
		t.Fatalf("should be set to 90%% of the limit: %d", got)
	}
	if cur := debug.SetMemoryLimit(-1); cur != 900000000 {
		t.Fatalf("runtime limit: %d", cur)
	}
	t.Setenv("GOMEMLIMIT", "256MiB")
	if got := SetMemoryLimit(slog.New(slog.DiscardHandler)); got != 0 {
		t.Fatal("should not override an existing GOMEMLIMIT environment variable")
	}
}
