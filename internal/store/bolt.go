package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	bucketData  = []byte("data")
	bucketAudit = []byte("audit")
	bucketHours = []byte("hours")
	keyData     = []byte("v1")
)

// Bolt 是 bbolt 文件存储。数据在内存里有一份，读不碰磁盘。
type Bolt struct {
	db   *bolt.DB
	mu   sync.Mutex // 串行化 Update
	data atomic.Pointer[Data]
	seq  atomic.Uint32
	now  func() time.Time

	auditMu    sync.Mutex
	auditCount int
	maxAudit   int
}

// OpenBolt 打开（或创建）数据库文件。另一个进程正在用时，2 秒后报错。
func OpenBolt(path string) (*Bolt, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("打开运营数据库 %s：%w（是不是另一个进程在用？）", path, err)
	}
	s := &Bolt{db: db, now: time.Now, maxAudit: maxAudit}
	var d Data
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketData, bucketAudit, bucketHours} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		if raw := tx.Bucket(bucketData).Get(keyData); raw != nil {
			return json.Unmarshal(raw, &d)
		}
		return nil
	})
	if err == nil {
		err = db.View(func(tx *bolt.Tx) error {
			s.auditCount = tx.Bucket(bucketAudit).Stats().KeyN
			return nil
		})
	}
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("读取运营数据库：%w", err)
	}
	s.data.Store(&d)
	return s, nil
}

func (s *Bolt) Data() Data { return s.data.Load().Clone() }

func (s *Bolt) Update(fn func(*Data) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.data.Load().Clone()
	if err := fn(&d); err != nil {
		return err
	}
	d.Updated = s.now().UTC()
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketData).Put(keyData, raw) }); err != nil {
		return err
	}
	s.data.Store(&d)
	return nil
}

func (s *Bolt) Audit(e AuditEntry) error {
	if e.Time.IsZero() {
		e.Time = s.now().UTC()
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	key := make([]byte, 12)
	binary.BigEndian.PutUint64(key, uint64(e.Time.UnixNano()))
	binary.BigEndian.PutUint32(key[8:], s.seq.Add(1))
	s.auditMu.Lock()
	defer s.auditMu.Unlock()
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketAudit)
		if err := b.Put(key, raw); err != nil {
			return err
		}
		s.auditCount++
		// 超过上限时删掉最旧的：先收集键再删，不在游标遍历中删除
		if extra := s.auditCount - s.maxAudit; extra > 0 {
			var old [][]byte
			c := b.Cursor()
			for k, _ := c.First(); k != nil && len(old) < extra; k, _ = c.Next() {
				old = append(old, append([]byte(nil), k...))
			}
			for _, k := range old {
				if err := b.Delete(k); err != nil {
					return err
				}
			}
			s.auditCount -= len(old)
		}
		return nil
	})
}

func (s *Bolt) AuditLog(limit int) []AuditEntry {
	var out []AuditEntry
	_ = s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketAudit).Cursor()
		for k, v := c.Last(); k != nil && len(out) < limit; k, v = c.Prev() {
			var e AuditEntry
			if json.Unmarshal(v, &e) == nil {
				out = append(out, e)
			}
		}
		return nil
	})
	return out
}

func hourKey(t time.Time) []byte {
	k := make([]byte, 8)
	binary.BigEndian.PutUint64(k, uint64(t.Truncate(time.Hour).Unix()))
	return k
}

func (s *Bolt) SaveHour(hour time.Time, b []byte) error {
	cut := hourKey(s.now().Add(-keepHours * time.Hour))
	return s.db.Update(func(tx *bolt.Tx) error {
		bk := tx.Bucket(bucketHours)
		if err := bk.Put(hourKey(hour), b); err != nil {
			return err
		}
		var old [][]byte
		c := bk.Cursor()
		for k, _ := c.First(); k != nil && string(k) < string(cut); k, _ = c.Next() {
			old = append(old, append([]byte(nil), k...))
		}
		for _, k := range old {
			if err := bk.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Bolt) HourStats(since time.Time) [][]byte {
	var out [][]byte
	start := hourKey(since)
	_ = s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket(bucketHours).Cursor()
		for k, v := c.Seek(start); k != nil; k, v = c.Next() {
			out = append(out, append([]byte(nil), v...))
		}
		return nil
	})
	return out
}

func (s *Bolt) Writable() bool { return true }

func (s *Bolt) Close() error { return s.db.Close() }
