package accesslog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMarshalCaddyFields(t *testing.T) {
	e := &Entry{
		Time: time.Unix(1792744703, 552000000), RemoteIP: "10.0.0.1", RemotePort: "41938", ClientIP: "198.51.100.23",
		Proto: "HTTP/1.1", Method: "GET", Host: "docker.linkos.org", URI: "/v2/library/nginx/blobs/sha256:ab?ns=docker.io",
		UserAgent: "containerd/2.1.4", Duration: 1500 * time.Millisecond, Size: 1234, Status: 200,
		RespContentLength: "1234", RespContentType: "application/octet-stream",
		Entrance: "docker", Upstream: "docker.io", Hops: []string{"production.cloudfront.docker.com"},
	}
	var m map[string]any
	if err := json.Unmarshal(Marshal(e), &m); err != nil {
		t.Fatal(err)
	}
	req := m["request"].(map[string]any)
	// abuse-report.py 用到的字段
	if m["ts"].(float64) != 1792744703.552 || req["host"] != "docker.linkos.org" || req["client_ip"] != "198.51.100.23" ||
		req["uri"] != e.URI || m["status"].(float64) != 200 || m["size"].(float64) != 1234 || m["duration"].(float64) != 1.5 {
		t.Fatalf("字段不对：%v", m)
	}
	if ua := req["headers"].(map[string]any)["User-Agent"].([]any); ua[0] != "containerd/2.1.4" {
		t.Fatalf("User-Agent：%v", ua)
	}
	if cl := m["resp_headers"].(map[string]any)["Content-Length"].([]any); cl[0] != "1234" {
		t.Fatalf("Content-Length：%v", cl)
	}
	if m["redirect1"] != "production.cloudfront.docker.com" || m["logger"] != "http.log.access" || m["msg"] != "handled request" {
		t.Fatalf("附加字段：%v", m)
	}
	if _, ok := m["redirect2"]; ok {
		t.Fatal("没有的跳数不应输出")
	}
}

type buf struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *buf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(p)
}
func (b *buf) Close() error { return nil }

func TestLoggerWritesAllOnClose(t *testing.T) {
	var out buf
	l := newLogger(&out)
	for i := 0; i < 1000; i++ {
		l.Log(&Entry{Time: time.Now(), Status: 200, URI: "/v2/"})
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "\n"); n+int(l.Dropped()) != 1000 {
		t.Fatalf("写出 %d 行，丢弃 %d 行", n, l.Dropped())
	}
	l.Log(&Entry{}) // 关闭后再写不应 panic
	var nilLogger *Logger
	nilLogger.Log(&Entry{})
	_ = nilLogger.Close()
}

func TestRotate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	r, err := newRotator(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	tick := time.Unix(1792744703, 0)
	r.now = func() time.Time { tick = tick.Add(time.Second); return tick }
	line := []byte(strings.Repeat("x", 39) + "\n") // 40 字节，每个文件放两行
	for i := 0; i < 9; i++ {
		if _, err := r.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.Close()
	old, _ := filepath.Glob(filepath.Join(dir, "access-*.log"))
	if len(old) != 2 {
		t.Fatalf("应只保留 2 个旧文件：%v", old)
	}
	all, _ := filepath.Glob(filepath.Join(dir, "access*.log"))
	if len(all) != 3 {
		t.Fatalf("access*.log 应能匹配当前文件和旧文件：%v", all)
	}
	f, _ := os.Open(path)
	defer f.Close()
	n := 0
	for s := bufio.NewScanner(f); s.Scan(); n++ {
	}
	if n != 1 {
		t.Fatalf("当前文件应有 1 行，实际 %d", n)
	}
}
