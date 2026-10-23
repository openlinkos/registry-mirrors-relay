package accesslog

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// rotator 按大小轮转日志文件：access.log 写满后改名为 access-<时间>.log，只保留最新的 keep 个旧文件。
// 旧文件的名字仍以 access 开头、.log 结尾，和 Caddy 一样，统计脚本按 access*.log 就能读全。
type rotator struct {
	mu   sync.Mutex
	path string
	max  int64
	keep int
	f    *os.File
	size int64
	now  func() time.Time
}

func newRotator(path string, max int64, keep int) (*rotator, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	r := &rotator{path: path, max: max, keep: keep, now: time.Now}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotator) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(p)) > r.max {
		if err := r.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotator) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	ext := filepath.Ext(r.path)
	base := strings.TrimSuffix(r.path, ext)
	// Windows 的文件名不能有冒号
	name := base + "-" + r.now().UTC().Format("2006-01-02T15-04-05.000") + ext
	if err := os.Rename(r.path, name); err != nil {
		return err
	}
	r.prune(base, ext)
	return r.open()
}

func (r *rotator) prune(base, ext string) {
	old, _ := filepath.Glob(base + "-*" + ext)
	if len(old) <= r.keep {
		return
	}
	sort.Strings(old) // 时间戳格式按字典序就是时间顺序
	for _, p := range old[:len(old)-r.keep] {
		_ = os.Remove(p)
	}
}

func (r *rotator) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
