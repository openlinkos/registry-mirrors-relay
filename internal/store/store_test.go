package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestBoltPersistsAndCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.db")
	s, err := OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update(func(d *Data) error {
		d.Users = append(d.Users, ClientUser{Name: "alice", Hash: "$2a$x"})
		d.Bans = append(d.Bans, Ban{CIDR: "198.51.100.7/32", Until: time.Now().Add(time.Hour)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d := s.Data()
	d.Users[0].Name = "mutated"
	if s.Data().Users[0].Name != "alice" {
		t.Fatal("Data 应返回副本")
	}
	// 回调出错时不写入
	if err := s.Update(func(d *Data) error { d.Users = nil; return errors.New("no") }); err == nil || len(s.Data().Users) != 1 {
		t.Fatal("回调出错时不应修改")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenBolt(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.Data(); len(got.Users) != 1 || got.Users[0].Name != "alice" || len(got.Bans) != 1 || got.Updated.IsZero() {
		t.Fatalf("重新打开后数据应还在：%+v", got)
	}
	// 另一个进程（这里是同一进程的第二次打开）拿不到锁
	if _, err := OpenBolt(path); err == nil {
		t.Fatal("文件被占用时应报错")
	}
}

func TestBoltAuditRetention(t *testing.T) {
	s, err := OpenBolt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.maxAudit = 200 // 每次写入都要 fsync，测试里用小一点的上限
	base := time.Unix(1792744703, 0)
	for i := 0; i < s.maxAudit+50; i++ {
		if err := s.Audit(AuditEntry{Time: base.Add(time.Duration(i) * time.Second), Actor: "t", Action: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	log := s.AuditLog(3)
	if len(log) != 3 || log[0].Action != fmt.Sprint(s.maxAudit+49) {
		t.Fatalf("最新的在前：%+v", log)
	}
	all := s.AuditLog(s.maxAudit * 2)
	if len(all) != s.maxAudit || all[len(all)-1].Action != "50" {
		t.Fatalf("应只保留最近 %d 条：%d，最旧 %s", s.maxAudit, len(all), all[len(all)-1].Action)
	}
}

func TestHours(t *testing.T) {
	for name, s := range map[string]Store{"bolt": mustBolt(t), "readonly": NewReadOnly()} {
		now := time.Unix(1792744703, 0)
		for i := 0; i < 60; i++ {
			if err := s.SaveHour(now.Add(-time.Duration(i)*time.Hour), []byte(fmt.Sprint(i))); err != nil {
				t.Fatal(err)
			}
		}
		got := s.HourStats(now.Add(-3 * time.Hour))
		if len(got) != 4 || string(got[0]) != "3" || string(got[3]) != "0" {
			t.Fatalf("%s：最近 4 小时，按时间顺序：%q", name, got)
		}
		_ = s.Close()
	}
}

func mustBolt(t *testing.T) *Bolt {
	s, err := OpenBolt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestReadOnly(t *testing.T) {
	s := NewReadOnly()
	if s.Writable() || !errors.Is(s.Update(func(*Data) error { return nil }), ErrReadOnly) {
		t.Fatal("只读存储不能修改")
	}
	for i := 0; i < memAudit+10; i++ {
		_ = s.Audit(AuditEntry{Action: fmt.Sprint(i)})
	}
	if l := s.AuditLog(memAudit * 2); len(l) != memAudit || l[0].Action != fmt.Sprint(memAudit+9) {
		t.Fatalf("内存审计：%d", len(l))
	}
}
