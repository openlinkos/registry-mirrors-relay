// Package accesslog writes the access log. Field names match Caddy's JSON access log, so existing
// analysis scripts (such as abuse-report.py) need no changes.
//
// Logging must not slow down pulls: entries go into a bounded queue and a separate goroutine writes
// them out; when the queue is full, entries are dropped and counted.
package accesslog

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"

	"sync"
	"sync/atomic"
	"time"

	"github.com/openlinkos/registry-mirrors-relay/internal/config"
)

// Entry is one access record.
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
	// Two response headers are recorded: Content-Length tells complete downloads from interrupted
	// ones, and Content-Type helps with troubleshooting
	RespContentLength string
	RespContentType   string

	Entrance string
	Upstream string
	Image    string   // full image name, e.g. docker.io/library/nginx
	Kind     string   // manifest, blob, tags, referrers, ping
	Ref      string   // tag or digest
	Hops     []string // each hop the server followed, written as redirect1..3
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
	Image       string              `json:"image,omitempty"`
	Kind        string              `json:"kind,omitempty"`
	Redirect1   string              `json:"redirect1,omitempty"`
	Redirect2   string              `json:"redirect2,omitempty"`
	Redirect3   string              `json:"redirect3,omitempty"`
	Cache       string              `json:"cache,omitempty"`
	Limited     bool                `json:"limited,omitempty"`
	Error       string              `json:"error,omitempty"`
}

// Marshal encodes a record as one line of JSON (without the newline).
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
		Entrance:    e.Entrance, Upstream: e.Upstream, Image: e.Image, Kind: e.Kind, Cache: e.Cache, Limited: e.Limited, Error: e.Error,
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

// Logger writes the log asynchronously.
type Logger struct {
	ch      chan []byte
	out     io.WriteCloser
	dropped atomic.Uint64
	wg      sync.WaitGroup
	closed  atomic.Bool
}

// queueSize is the limit on pending log entries (in lines), roughly a few MB.
const queueSize = 8192

// New creates a Logger from the configuration. When output is off it returns nil; callers can safely
// call Log on a nil Logger.
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

// Log records one entry. When the queue is full the entry is dropped rather than blocking the request.
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

// Dropped is the number of entries dropped because the queue was full.
func (l *Logger) Dropped() uint64 {
	if l == nil {
		return 0
	}
	return l.dropped.Load()
}

// Close writes out the entries left in the queue and closes the output.
func (l *Logger) Close() error {
	if l == nil || !l.closed.CompareAndSwap(false, true) {
		return nil
	}
	close(l.ch)
	l.wg.Wait()
	return l.out.Close()
}

type noteKey struct{}

// WithNote attaches a short note to this request's access record (written to the error field), for
// example why the management entrance refused the request.
func WithNote(ctx context.Context, note string) context.Context {
	return context.WithValue(ctx, noteKey{}, note)
}

// NoteFrom returns the note attached by WithNote.
func NoteFrom(ctx context.Context) string {
	s, _ := ctx.Value(noteKey{}).(string)
	return s
}
