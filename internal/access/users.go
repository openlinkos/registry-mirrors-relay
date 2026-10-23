// Package access 是访问控制：客户端账号（HTTP Basic）、IP 黑白名单和临时封禁、镜像黑白名单。
// 规则来自配置文件和运营数据库，合并后整体替换；校验口令的缓存跨替换保留。
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

// User 是一个客户端账号。
type User struct {
	Name        string
	Hash        []byte // bcrypt
	Disabled    bool
	Source      string  // config（来自 users_file）或 store（来自运营数据库）
	ManifestRPS float64 // 单独的限流，0 表示用默认值
	BlobStreams int
}

// ValidHash 判断是不是 bcrypt 哈希。其他格式（MD5、SHA1、明文）一律不接受。
func ValidHash(h string) bool {
	if !strings.HasPrefix(h, "$2a$") && !strings.HasPrefix(h, "$2b$") && !strings.HasPrefix(h, "$2y$") {
		return false
	}
	_, err := bcrypt.Cost([]byte(h))
	return err == nil
}

// ParseHtpasswd 读取 htpasswd 文件：每行 用户名:bcrypt 哈希，# 开头的行和空行忽略。
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
			return nil, fmt.Errorf("第 %d 行：应写成 用户名:哈希", line)
		}
		if !ValidHash(hash) {
			return nil, fmt.Errorf("第 %d 行（%s）：只接受 bcrypt 哈希，可以用 registry-mirrors-relay hash-password 生成", line, name)
		}
		if seen[name] {
			return nil, fmt.Errorf("第 %d 行：用户 %s 重复", line, name)
		}
		seen[name] = true
		out = append(out, User{Name: name, Hash: []byte(hash), Source: "config"})
	}
	return out, s.Err()
}

// Authenticator 校验口令。bcrypt 每次要几十毫秒，所以：
//   - 校验成功的结果按 HMAC(进程密钥, 用户名、口令、哈希) 缓存 10 分钟，改了口令或哈希自然失效；
//   - 同时在跑的 bcrypt 不超过 CPU 核数，排不上就直接拒绝（防止用登录请求把 CPU 吃满）。
type Authenticator struct {
	key   []byte
	cache *lru.Cache[[32]byte, time.Time]
	sem   chan struct{}
	ttl   time.Duration
	now   func() time.Time
}

// NewAuthenticator 创建校验器。
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

// ErrBusy 表示同时校验的口令太多。
var ErrBusy = errors.New("同时校验的口令太多")

// Verify 校验 user 的口令。返回 ErrBusy 时调用方应返回 503（不算一次登录失败）。
func (a *Authenticator) Verify(u *User, password string) (bool, error) {
	if u == nil || u.Disabled {
		// 用户不存在也做一次等价的 bcrypt，避免按耗时区分用户是否存在
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

// dummyHash 是一个固定口令的 bcrypt 哈希（cost 12），只用来让"用户不存在"和"口令错误"耗时相近。
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
