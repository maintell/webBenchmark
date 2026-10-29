package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	distSessionCookie = "wb_sid"
	// 所有改状态的请求都必须带上这个头。它让"用户被诱导点了一个链接
	// 就悄悄开始压测"这类 CSRF 变成一个不可能的事件。
	distCSRFHeader     = "X-WB-CSRF"
	distSessionTTL     = 12 * time.Hour
	distMaxLoginFails  = 5
	distLoginBlockBase = time.Minute
	distLoginBlockMax  = 15 * time.Minute
	distMinPasswordLen = 10
	distGeneratedPwLen = 18
)

// passwordAlphabet 去掉了容易看错的 0/O/1/l/I，
// 因为主控密码经常要靠肉眼从日志里抄到别的机器上。
const passwordAlphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// adminSession 是控制台登录态。csrf 与 cookie 一一绑定，
// 只存在服务端，页面每次加载都重新取。
type adminSession struct {
	csrf    string
	expires time.Time
}

type sessionStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]*adminSession
}

func newSessionStore(ttl time.Duration) *sessionStore {
	return &sessionStore{ttl: ttl, m: make(map[string]*adminSession)}
}

func (s *sessionStore) create() (token, csrf string) {
	token = randomHex(32)
	csrf = randomHex(24)
	now := time.Now()
	s.mu.Lock()
	s.m[sha256Hex([]byte(token))] = &adminSession{csrf: csrf, expires: now.Add(s.ttl)}
	s.gcLocked(now)
	s.mu.Unlock()
	return token, csrf
}

// lookup 顺便做滑动续期：运维在控制台上盯着一轮压测时会不断有请求，
// 中途被踢回登录页反而更容易让人把密码存到浏览器里。
func (s *sessionStore) lookup(token string) (*adminSession, bool) {
	if token == "" {
		return nil, false
	}
	key := sha256Hex([]byte(token))
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[key]
	if !ok {
		return nil, false
	}
	if now.After(sess.expires) {
		delete(s.m, key)
		return nil, false
	}
	sess.expires = now.Add(s.ttl)
	return sess, true
}

func (s *sessionStore) drop(token string) {
	key := sha256Hex([]byte(token))
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
}

func (s *sessionStore) gcLocked(now time.Time) {
	if len(s.m) < 256 {
		return
	}
	for k, v := range s.m {
		if now.After(v.expires) {
			delete(s.m, k)
		}
	}
}

// limitBucket 记录某个来源 IP 的尝试次数与封锁截止时间。
type limitBucket struct {
	count      int
	reset      time.Time
	fails      int
	blockUntil time.Time
}

// rateLimiter 是"窗口内最多 N 次"的计数限流，登录失败还会指数退避。
// 它按来源 IP 记账，不信任 X-Forwarded-For —— 那个头是客户端可控的。
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*limitBucket
	max     int
	window  time.Duration
}

func newRateLimiter(max int, window time.Duration) *rateLimiter {
	return &rateLimiter{buckets: make(map[string]*limitBucket), max: max, window: window}
}

// blocked 返回还需要等多久才能再试，0 表示没有被封锁。
func (l *rateLimiter) blocked(key string) time.Duration {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil || b.blockUntil.IsZero() || now.After(b.blockUntil) {
		return 0
	}
	return b.blockUntil.Sub(now)
}

// allow 在窗口内计数，超过上限返回 false。
func (l *rateLimiter) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.gcLocked(now)
	b := l.buckets[key]
	if b == nil {
		b = &limitBucket{}
		l.buckets[key] = b
	}
	if now.After(b.reset) {
		b.count = 0
		b.reset = now.Add(l.window)
	}
	b.count++
	return b.count <= l.max
}

// fail 记录一次失败，返回本次新施加的封锁时长。
func (l *rateLimiter) fail(key string) time.Duration {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		b = &limitBucket{}
		l.buckets[key] = b
	}
	b.fails++
	if b.fails < distMaxLoginFails {
		return 0
	}
	over := b.fails - distMaxLoginFails
	d := distLoginBlockBase << uint(over)
	if d > distLoginBlockMax || d <= 0 {
		d = distLoginBlockMax
	}
	b.blockUntil = now.Add(d)
	return d
}

func (l *rateLimiter) clear(key string) {
	l.mu.Lock()
	delete(l.buckets, key)
	l.mu.Unlock()
}

func (l *rateLimiter) gcLocked(now time.Time) {
	if len(l.buckets) < 1024 {
		return
	}
	for k, b := range l.buckets {
		if now.After(b.reset) && now.After(b.blockUntil) {
			delete(l.buckets, k)
		}
	}
}

// auditLog 是只追加的 JSONL 审计日志。
// 入网、吊销、签发令牌、登录成功/失败、任务开始/停止都写进去：
// 这些是"谁在什么时候让哪些机器打了什么目标"的唯一凭据。
type auditLog struct {
	mu sync.Mutex
	f  *os.File
}

func newAuditLog(path string) (*auditLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开审计日志 %s: %w", path, err)
	}
	return &auditLog{f: f}, nil
}

func (a *auditLog) event(kind string, kv map[string]any) {
	line := make(map[string]any, len(kv)+2)
	for k, v := range kv {
		line[k] = v
	}
	line["t"] = time.Now().UTC().Format(time.RFC3339)
	line["event"] = kind
	data, err := json.Marshal(line)
	if err != nil {
		return
	}
	a.mu.Lock()
	_, _ = a.f.Write(append(data, '\n'))
	a.mu.Unlock()
}

func (a *auditLog) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.f.Close()
}

// loadOrCreateAdminHash 读取控制台密码的 bcrypt 哈希；首次启动时生成强密码。
// override 非空表示运维显式指定了密码（自动化部署常用），此时返回的 generated 为空。
func loadOrCreateAdminHash(dir, override string) (hash []byte, generated string, err error) {
	// 首次启动时数据目录还不存在，而密码哈希是第一个落盘的文件；
	// 少了这一步，-data 指向新目录的主控会在写哈希时直接失败。
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("创建数据目录 %s: %w", dir, err)
	}
	path := filepath.Join(dir, "admin.hash")

	if override != "" {
		if len(override) < distMinPasswordLen {
			return nil, "", fmt.Errorf("主控密码至少 %d 位", distMinPasswordLen)
		}
		h, err := bcrypt.GenerateFromPassword([]byte(override), bcrypt.DefaultCost)
		if err != nil {
			return nil, "", err
		}
		old, _ := os.ReadFile(path)
		if string(old) != string(h) {
			if err := writeFileAtomic(path, h, 0o600); err != nil {
				return nil, "", err
			}
		}
		return h, "", nil
	}

	if existing, err := os.ReadFile(path); err == nil && len(existing) > 0 {
		return existing, "", nil
	}

	pw, err := randomPassword(distGeneratedPwLen)
	if err != nil {
		return nil, "", err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return nil, "", err
	}
	if err := writeFileAtomic(path, h, 0o600); err != nil {
		return nil, "", err
	}
	return h, pw, nil
}

func randomPassword(n int) (string, error) {
	var b strings.Builder
	max := big.NewInt(int64(len(passwordAlphabet)))
	for i := 0; i < n; i++ {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b.WriteByte(passwordAlphabet[idx.Int64()])
	}
	return b.String(), nil
}

func checkAdminPassword(hash []byte, password string) bool {
	if len(hash) == 0 || password == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(password)) == nil
}

// bearerHash 仅用于日志里标识"是哪个会话"，不打印会话令牌本身。
func bearerHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}
