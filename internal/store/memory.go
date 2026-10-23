package store

import (
	"sort"
	"sync"
	"time"
)

// ReadOnly 是 driver: none 的实现：运营数据为空（全部来自配置文件），不能修改；
// 审计和每小时统计只在内存里保留最近一段，重启后清空。
type ReadOnly struct {
	mu    sync.Mutex
	audit []AuditEntry
	hours map[int64][]byte
	now   func() time.Time
}

// NewReadOnly 创建只读存储。
func NewReadOnly() *ReadOnly { return &ReadOnly{hours: map[int64][]byte{}, now: time.Now} }

func (s *ReadOnly) Data() Data { return Data{} }

func (s *ReadOnly) Update(func(*Data) error) error { return ErrReadOnly }

func (s *ReadOnly) Audit(e AuditEntry) error {
	if e.Time.IsZero() {
		e.Time = s.now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = append(s.audit, e)
	if len(s.audit) > memAudit {
		s.audit = s.audit[len(s.audit)-memAudit:]
	}
	return nil
}

func (s *ReadOnly) AuditLog(limit int) []AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AuditEntry
	for i := len(s.audit) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.audit[i])
	}
	return out
}

func (s *ReadOnly) SaveHour(hour time.Time, b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hours[hour.Truncate(time.Hour).Unix()] = append([]byte(nil), b...)
	if len(s.hours) > memKeepHour {
		keys := make([]int64, 0, len(s.hours))
		for k := range s.hours {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, k := range keys[:len(keys)-memKeepHour] {
			delete(s.hours, k)
		}
	}
	return nil
}

func (s *ReadOnly) HourStats(since time.Time) [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]int64, 0, len(s.hours))
	for k := range s.hours {
		if k >= since.Truncate(time.Hour).Unix() {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([][]byte, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.hours[k])
	}
	return out
}

func (s *ReadOnly) Writable() bool { return false }

func (s *ReadOnly) Close() error { return nil }
