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
		t.Fatal("Data should return a copy")
	}
	// Nothing is written when the callback fails
	if err := s.Update(func(d *Data) error { d.Users = nil; return errors.New("no") }); err == nil || len(s.Data().Users) != 1 {
		t.Fatal("a failing callback should not modify anything")
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
		t.Fatalf("the data should still be there after reopening: %+v", got)
	}
	// Another process (here, a second open in the same process) cannot get the lock
	if _, err := OpenBolt(path); err == nil {
		t.Fatal("should fail while the file is in use")
	}
}

func TestBoltAuditRetention(t *testing.T) {
	s, err := OpenBolt(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.maxAudit = 200 // every write needs an fsync, so tests use a smaller limit
	base := time.Unix(1792744703, 0)
	for i := 0; i < s.maxAudit+50; i++ {
		if err := s.Audit(AuditEntry{Time: base.Add(time.Duration(i) * time.Second), Actor: "t", Action: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	log := s.AuditLog(3)
	if len(log) != 3 || log[0].Action != fmt.Sprint(s.maxAudit+49) {
		t.Fatalf("newest first: %+v", log)
	}
	all := s.AuditLog(s.maxAudit * 2)
	if len(all) != s.maxAudit || all[len(all)-1].Action != "50" {
		t.Fatalf("only the latest %d entries should be kept: %d, oldest %s", s.maxAudit, len(all), all[len(all)-1].Action)
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
			t.Fatalf("%s: last 4 hours, in chronological order: %q", name, got)
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
		t.Fatal("a read-only store must not be modifiable")
	}
	for i := 0; i < memAudit+10; i++ {
		_ = s.Audit(AuditEntry{Action: fmt.Sprint(i)})
	}
	if l := s.AuditLog(memAudit * 2); len(l) != memAudit || l[0].Action != fmt.Sprint(memAudit+9) {
		t.Fatalf("in-memory audit log: %d", len(l))
	}
}
