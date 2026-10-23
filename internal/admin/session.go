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

// 会话的期限，和 Origin 的后台一样。
const (
	sessionIdle   = 30 * time.Minute // 这么久没有操作就要重新登录
	sessionMax    = 12 * time.Hour   // 从登录起最长这么久
	freshWindow   = 10 * time.Minute // 敏感操作要求这么久之内输入过口令
	maxSessions   = 1000
	sessionIDSize = 32
)

// session 是一个登录会话。Cookie 里是随机 ID，这里只存它的 SHA-256；会话只在进程里，重启后要重新登录。
type session struct {
	idHash   string
	Handle   string // 哈希的前 16 位，在页面上标识会话
	Admin    string
	Role     string // admin | viewer
	CSRF     string
	Created  time.Time
	LastSeen time.Time
	FreshAt  time.Time
	IP       string
	Agent    string
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

// create 开始一个新会话，返回放进 Cookie 的 ID。会话满了时去掉最久没用的。
func (s *sessions) create(admin, role, ip, agent string, now time.Time) (string, *session) {
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
	x := &session{idHash: h, Handle: h[:16], Admin: admin, Role: role, CSRF: randomToken(),
		Created: now, LastSeen: now, FreshAt: now, IP: ip, Agent: agent}
	s.m[h] = x
	return id, x
}

// get 按 Cookie 里的 ID 找会话，找到时更新最后活动时间。返回副本。
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

// endFor 结束一个管理员的全部会话（除了 keep），返回结束的个数。
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

// endHandle 按页面上的标识结束一个会话。
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

// list 返回所有有效的会话，最近活动的在前。
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

// safeEqual 是常数时间的字符串比较。
func safeEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// ---- 登录限速

// loginLimits 是口令校验失败的上限：每个客户端、全局各自在滑动窗口内最多失败这么多次。
// 到了上限后所有口令校验都直接拒绝（口令正确也一样），直到最早的一次失败移出窗口。
type loginLimits struct {
	PerClient  int
	Global     int
	Window     time.Duration
	MaxClients int
}

var defaultLoginLimits = loginLimits{PerClient: 10, Global: 100, Window: 10 * time.Minute, MaxClients: 10000}

type failBucket struct {
	fails  []time.Time
	logged bool // 这个窗口里的封锁是否已记过审计
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

// check 判断现在能不能校验口令。不能时返回要等多久，以及这是不是这个窗口里第一次被拦（用来只记一次审计）。
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

// fail 记一次失败，返回这个客户端在窗口内的失败次数（含这一次）。
func (l *loginLimiter) fail(client string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.bucket(client, now)
	b.fails = append(b.fails, now)
	l.prune(&l.all, now)
	l.all.fails = append(l.all.fails, now)
	return len(b.fails)
}

// checkQueue 让口令校验一个一个地跑：bcrypt 故意很慢，串行既限制了 CPU，也让限速是精确的
// （一批并发的猜测不可能都在第一次失败被记下之前通过检查）。最多 maxWaiting 个在排队，再多就直接拒绝。
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
