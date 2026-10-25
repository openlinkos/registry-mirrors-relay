// Package access is access control: client accounts (HTTP Basic), IP allow and deny lists with temporary
// bans, and image allow and deny lists. Rules come from the configuration file and the operational database,
// are merged and replaced as a whole; the password check cache survives replacement.
package access

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/openlinkos/registry-mirrors-relay/internal/lru"
)

// User is a client account.
type User struct {
	Name        string
	Hash        []byte // bcrypt
	Disabled    bool
	Source      string  // config (from users_file) or store (from the operational database)
	ManifestRPS float64 // per-account rate limit, 0 means the default
	BlobStreams int
}

// ValidHash reports whether h is a bcrypt hash. Other formats (MD5, SHA1, plain text) are never accepted.
func ValidHash(h string) bool {
	if !strings.HasPrefix(h, "$2a$") && !strings.HasPrefix(h, "$2b$") && !strings.HasPrefix(h, "$2y$") {
		return false
	}
	_, err := bcrypt.Cost([]byte(h))
	return err == nil
}

// ParseHtpasswd reads an htpasswd file: one username:bcrypt-hash per line; lines starting with # and empty
// lines are ignored.
func ParseHtpasswd(r io.Reader) ([]User, error) {
	var out []User
	seen := map[string]bool{}
	s := bufio.NewScanner(r)
	line := 0
	for s.Scan() {
		line++
		t := strings.TrimSpace(s.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		name, hash, ok := strings.Cut(t, ":")
		if !ok || name == "" {
			return nil, fmt.Errorf("line %d: expected username:hash", line)
		}
		if !ValidHash(hash) {
			return nil, fmt.Errorf("line %d (%s): only bcrypt hashes are accepted; generate one with registry-mirrors-relay hash-password", line, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("line %d: duplicate user %s", line, name)
		}
		seen[name] = true
		out = append(out, User{Name: name, Hash: []byte(hash), Source: "config"})
	}
	return out, s.Err()
}

// Authenticator checks passwords. Each bcrypt check takes tens of milliseconds, so:
//   - successful checks are cached for 10 minutes under HMAC(process key, username, password, hash), so a
//     changed password or hash invalidates them naturally;
//   - no more bcrypt checks run at once than there are CPU cores, and a check that cannot get a slot is
//     rejected outright (so sign-in requests cannot saturate the CPU).
type Authenticator struct {
	key   []byte
	cache *lru.Cache[[32]byte, time.Time]
	sem   chan struct{}
	ttl   time.Duration
	now   func() time.Time
}

// NewAuthenticator creates an Authenticator.
func NewAuthenticator() *Authenticator {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return &Authenticator{
		key:   key,
		cache: lru.New[[32]byte, time.Time](16384, 0),
		sem:   make(chan struct{}, max(2, runtime.NumCPU())),
		ttl:   10 * time.Minute,
		now:   time.Now,
	}
}

// ErrBusy means too many passwords are being checked at once.
var ErrBusy = errors.New("too many concurrent password checks")

// Verify checks the user's password. When it returns ErrBusy, the caller should return 503 (this does not
// count as a failed sign-in).
func (a *Authenticator) Verify(u *User, password string) (bool, error) {
	if u == nil || u.Disabled {
		// Run an equivalent bcrypt even if the user does not exist, so timing does not reveal whether it exists
		a.dummy(password)
		return false, nil
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(u.Name))
	mac.Write([]byte{0})
	mac.Write([]byte(password))
	mac.Write([]byte{0})
	mac.Write(u.Hash)
	var k [32]byte
	copy(k[:], mac.Sum(nil))
	if at, ok := a.cache.Get(k); ok && a.now().Sub(at) < a.ttl {
		return true, nil
	}
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
	default:
		return false, ErrBusy
	}
	if bcrypt.CompareHashAndPassword(u.Hash, []byte(password)) != nil {
		return false, nil
	}
	a.cache.Add(k, a.now(), 0)
	return true, nil
}

// dummyHash is a bcrypt hash of a fixed password (cost 12), used only to make "user does not exist" and
// "wrong password" take a similar time.
var dummyHash = func() []byte {
	h, _ := bcrypt.GenerateFromPassword([]byte("registry-mirrors-relay"), 12)
	return h
}()

func (a *Authenticator) dummy(password string) {
	select {
	case a.sem <- struct{}{}:
		defer func() { <-a.sem }()
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
	default:
	}
}
