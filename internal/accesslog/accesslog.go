// Package accesslog 写访问日志。字段名和 Caddy 的 JSON 访问日志一致，已有的统计脚本（如 abuse-report.py）不用改。
//
// 写日志不能拖慢拉取：日志先进一个有上限的队列，由单独的协程写出；队列满了就丢弃并计数。
package accesslog

import (
	"bufio"
	"encoding/json"
	"io"
	"os"

	"sync"
	"sync/atomic"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

// Entry 是一条访问记录。
type Entry struct {
	Time       time.Time
	RemoteIP   string
	RemotePort string
	ClientIP   string
	Proto      string
	Method     string
	Host       string
	URI        string
	UserAgent  string
	UserID     string
	Duration   time.Duration
	Size       int64
	Status     int
	// 响应头里记两项：Content-Length 用来区分完整下载和中途断开，Content-Type 方便排查
	RespContentLength string
	RespContentType   string

	Entrance string
	Upstream string
	Hops     []string // 服务端跟随的每一跳，写成 redirect1..3
	Cache    string
	Limited  bool
	Error    string
}

type reqJSON struct {
	RemoteIP   string              `json:"remote_ip"`
	RemotePort string              `json:"remote_port"`
	ClientIP   string              `json:"client_ip"`
	Proto      string              `json:"proto"`
	Method     string              `json:"method"`
	Host       string              `json:"host"`
	URI        string              `json:"uri"`
	Headers    map[string][]string `json:"headers"`
}

type lineJSON struct {
	Level       string              `json:"level"`
	TS          float64             `json:"ts"`
	Logger      string              `json:"logger"`
	Msg         string              `json:"msg"`
	Request     reqJSON             `json:"request"`
	BytesRead   int                 `json:"bytes_read"`
	UserID      string              `json:"user_id"`
	Duration    float64             `json:"duration"`
	Size        int64               `json:"size"`
	Status      int                 `json:"status"`
	RespHeaders map[string][]string `json:"resp_headers"`
	Entrance    string              `json:"entrance,omitempty"`
	Upstream    string              `json:"upstream,omitempty"`
	Redirect1   string              `json:"redirect1,omitempty"`
	Redirect2   string              `json:"redirect2,omitempty"`
	Redirect3   string              `json:"redirect3,omitempty"`
	Cache       string              `json:"cache,omitempty"`
	Limited     bool                `json:"limited,omitempty"`
	Error       string              `json:"error,omitempty"`
}

// Marshal 把一条记录编码成一行 JSON（不含换行）。
func Marshal(e *Entry) []byte {
	l := lineJSON{
		Level: "info", Logger: "http.log.access", Msg: "handled request",
		TS: float64(e.Time.UnixNano()) / 1e9,
		Request: reqJSON{
			RemoteIP: e.RemoteIP, RemotePort: e.RemotePort, ClientIP: e.ClientIP, Proto: e.Proto,
			Method: e.Method, Host: e.Host, URI: e.URI, Headers: map[string][]string{},
		},
		UserID: e.UserID, Duration: e.Duration.Seconds(), Size: e.Size, Status: e.Status,
		RespHeaders: map[string][]string{},
		Entrance:    e.Entrance, Upstream: e.Upstream, Cache: e.Cache, Limited: e.Limited, Error: e.Error,
	}
	if e.UserAgent != "" {
		l.Request.Headers["User-Agent"] = []string{e.UserAgent}
	}
	if e.RespContentLength != "" {
		l.RespHeaders["Content-Length"] = []string{e.RespContentLength}
	}
	if e.RespContentType != "" {
		l.RespHeaders["Content-Type"] = []string{e.RespContentType}
	}
	for i, h := range e.Hops {
		switch i {
		case 0:
			l.Redirect1 = h
		case 1:
			l.Redirect2 = h
		case 2:
			l.Redirect3 = h
		}
	}
	b, _ := json.Marshal(l)
	return b
}

// Logger 异步写日志。
type Logger struct {
	ch      chan []byte
	out     io.WriteCloser
	dropped atomic.Uint64
	wg      sync.WaitGroup
	closed  atomic.Bool
}

// queueSize 是待写日志的上限（行数），大约几 MB。
const queueSize = 8192

// New 按配置创建 Logger。output 为 off 时返回 nil，调用方的 Log 也能安全地传 nil。
func New(cfg config.AccessLog) (*Logger, error) {
	var out io.WriteCloser
	switch cfg.Output {
	case "off":
		return nil, nil
	case "file":
		r, err := newRotator(cfg.Path, int64(cfg.MaxSize), cfg.Keep)
		if err != nil {
			return nil, err
		}
		out = r
	default:
		out = nopCloser{os.Stdout}
	}
	return newLogger(out), nil
}

func newLogger(out io.WriteCloser) *Logger {
	l := &Logger{ch: make(chan []byte, queueSize), out: out}
	l.wg.Add(1)
	go l.loop()
	return l
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func (l *Logger) loop() {
	defer l.wg.Done()
	bw := bufio.NewWriterSize(l.out, 64<<10)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case b, ok := <-l.ch:
			if !ok {
				_ = bw.Flush()
				return
			}
			_, _ = bw.Write(b)
			_ = bw.WriteByte('\n')
			if len(l.ch) == 0 && bw.Buffered() > 32<<10 {
				_ = bw.Flush()
			}
		case <-tick.C:
			_ = bw.Flush()
		}
	}
}

// Log 记一条。队列满了就丢弃，不阻塞请求。
func (l *Logger) Log(e *Entry) {
	if l == nil || l.closed.Load() {
		return
	}
	select {
	case l.ch <- Marshal(e):
	default:
		l.dropped.Add(1)
	}
}

// Dropped 是因为队列满而丢弃的条数。
func (l *Logger) Dropped() uint64 {
	if l == nil {
		return 0
	}
	return l.dropped.Load()
}

// Close 写完队列里剩下的日志并关闭输出。
func (l *Logger) Close() error {
	if l == nil || !l.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(l.ch)
	l.wg.Wait()
	return l.out.Close()
}
