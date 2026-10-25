package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// Session lifetimes.
const (
	sessionIdle   = 30 * time.Minute // after this long without activity, log in again
	sessionMax    = 12 * time.Hour   // at most this long from login
	freshWindow   = 10 * time.Minute // sensitive operations need the password entered within this time
	maxSessions   = 1000
	sessionIDSize = 32
)

// session is a login session. The cookie holds a random ID and only its SHA-256 is stored here; sessions
// live only in the process, so a restart requires logging in again.
type session struct {
	idHash string
	Handle string // the first 16 digits of the hash, identifying the session on pages
	Admin  string
	Role   string // admin | viewer
	// MustChange: the password was set by someone else and must be changed first; until then only
	// "My account" can be opened
	MustChange bool
	CSRF       string
	Created    time.Time
	LastSeen   time.Time
	FreshAt    time.Time
	IP         string
	Agent      string
}

type sessions struct {
	mu sync.Mutex
	m  map[string]*session
}

func newSessions() *sessions { return &sessions{m: map[string]*session{}} }

func randomToken() string {
	b := make([]byte, sessionIDSize)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashID(id string) string {
	s := sha256.Sum256([]byte(id))
	return hex.EncodeToString(s[:])
}

func (s *sessions) expiredLocked(x *session, now time.Time) bool {
	return now.Sub(x.Created) >= sessionMax || now.Sub(x.LastSeen) >= sessionIdle
}

// create starts a new session and returns the ID to put in the cookie. When the sessions are full, the
// one unused for longest is removed.
func (s *sessions) create(admin, role string, mustChange bool, ip, agent string, now time.Time) (string, *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, x := range s.m {
		if s.expiredLocked(x, now) {
			delete(s.m, k)
		}
	}
	if len(s.m) >= maxSessions {
		var oldest *session
		for _, x := range s.m {
			if oldest == nil || x.LastSeen.Before(oldest.LastSeen) {
				oldest = x
			}
		}
		delete(s.m, oldest.idHash)
	}
	if len(agent) > 256 {
		agent = agent[:256]
	}
	id := randomToken()
	h := hashID(id)
	x := &session{idHash: h, Handle: h[:16], Admin: admin, Role: role, MustChange: mustChange, CSRF: randomToken(),
		Created: now, LastSeen: now, FreshAt: now, IP: ip, Agent: agent}
	s.m[h] = x
	return id, x
}

// get finds the session by the ID in the cookie and, when found, updates its last activity time. It
// returns a copy.
func (s *sessions) get(id string, now time.Time) *session {
	if id == "" || len(id) > 128 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.m[hashID(id)]
	if !ok {
		return nil
	}
	if s.expiredLocked(x, now) {
		delete(s.m, x.idHash)
		return nil
	}
	x.LastSeen = now
	c := *x
	return &c
}

func (s *sessions) end(idHash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, idHash)
}

// endFor ends all sessions of an administrator (except keep) and returns how many were ended.
func (s *sessions) endFor(admin, keep string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for k, x := range s.m {
		if x.Admin == admin && k != keep {
			delete(s.m, k)
			n++
		}
	}
	return n
}

// endHandle ends a session by the handle shown on pages.
func (s *sessions) endHandle(handle string) (*session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, x := range s.m {
		if x.Handle == handle {
			delete(s.m, k)
			c := *x
			return &c, true
		}
	}
	return nil, false
}

func (s *sessions) markFresh(idHash string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if x, ok := s.m[idHash]; ok {
		x.FreshAt = now
	}
}

func (x *session) fresh(now time.Time) bool { return now.Sub(x.FreshAt) < freshWindow }

// list returns all valid sessions, most recently active first.
func (s *sessions) list(now time.Time) []session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]session, 0, len(s.m))
	for _, x := range s.m {
		if !s.expiredLocked(x, now) {
			out = append(out, *x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastSeen.After(out[j].LastSeen) })
	return out
}

// safeEqual is a constant-time string comparison.
func safeEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---- Login rate limits

// loginLimits caps failed password checks: per client and overall, each allows at most this many
// failures within a sliding window. Once a cap is reached every password check is rejected outright
// (even with the right password) until the earliest failure leaves the window.
type loginLimits struct {
	PerClient  int
	Global     int
	Window     time.Duration
	MaxClients int
}

var defaultLoginLimits = loginLimits{PerClient: 10, Global: 100, Window: 10 * time.Minute, MaxClients: 10000}

type failBucket struct {
	fails  []time.Time
	logged bool // whether the block in this window has already been written to the audit log
}

type loginLimiter struct {
	mu      sync.Mutex
	o       loginLimits
	clients map[string]*failBucket
	all     failBucket
}

func newLoginLimiter(o loginLimits) *loginLimiter {
	return &loginLimiter{o: o, clients: map[string]*failBucket{}}
}

func (l *loginLimiter) prune(b *failBucket, now time.Time) {
	cut := 0
	for cut < len(b.fails) && now.Sub(b.fails[cut]) >= l.o.Window {
		cut++
	}
	b.fails = b.fails[cut:]
	if len(b.fails) == 0 {
		b.logged = false
	}
}

func (l *loginLimiter) bucket(client string, now time.Time) *failBucket {
	b, ok := l.clients[client]
	if !ok {
		if len(l.clients) >= l.o.MaxClients {
			for k, v := range l.clients {
				l.prune(v, now)
				if len(v.fails) == 0 || len(l.clients) >= l.o.MaxClients {
					delete(l.clients, k)
				}
				if len(l.clients) < l.o.MaxClients {
					break
				}
			}
		}
		b = &failBucket{}
		l.clients[client] = b
	}
	l.prune(b, now)
	return b
}

// check decides whether a password can be checked now. If not, it returns how long to wait and whether
// this is the first block in the window (so that the audit log records it only once).
func (l *loginLimiter) check(client string, now time.Time) (ok bool, wait time.Duration, first bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(&l.all, now)
	b := l.bucket(client, now)
	var blocked []*failBucket
	if len(b.fails) >= l.o.PerClient {
		blocked = append(blocked, b)
	}
	if len(l.all.fails) >= l.o.Global {
		blocked = append(blocked, &l.all)
	}
	if len(blocked) == 0 {
		return true, 0, false
	}
	for _, x := range blocked {
		if until := x.fails[0].Add(l.o.Window).Sub(now); until > wait {
			wait = until
		}
		if !x.logged {
			first = true
			x.logged = true
		}
	}
	return false, max(wait, time.Second), first
}

// fail records a failure and returns the client's number of failures in the window (including this one).
func (l *loginLimiter) fail(client string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(client, now)
	b.fails = append(b.fails, now)
	l.prune(&l.all, now)
	l.all.fails = append(l.all.fails, now)
	return len(b.fails)
}

// checkQueue runs password checks one at a time: bcrypt is deliberately slow, and running checks in
// series both limits CPU use and makes the rate limit exact (a batch of concurrent guesses cannot all
// pass the check before the first failure is recorded). At most maxWaiting wait in the queue; any more
// are rejected outright.
type checkQueue struct {
	run     sync.Mutex
	mu      sync.Mutex
	waiting int
	max     int
}

func (q *checkQueue) do(fn func()) bool {
	q.mu.Lock()
	if q.waiting >= q.max {
		q.mu.Unlock()
		return false
	}
	q.waiting++
	q.mu.Unlock()
	q.run.Lock()
	defer func() {
		q.run.Unlock()
		q.mu.Lock()
		q.waiting--
		q.mu.Unlock()
	}()
	fn()
	return true
}
