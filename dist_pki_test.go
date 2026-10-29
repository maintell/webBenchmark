package main

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustPKI(t *testing.T, dir string) *pki {
	t.Helper()
	p, err := newPKI(dir)
	if err != nil {
		t.Fatalf("newPKI(%s): %v", dir, err)
	}
	return p
}

func TestPKIReusesCAOnRestart(t *testing.T) {
	dir := t.TempDir()
	p1 := mustPKI(t, dir)
	p2 := mustPKI(t, dir)
	if p1.fp != p2.fp {
		t.Fatalf("CA fingerprint changed across restart: %s vs %s", p1.fp, p2.fp)
	}
	if len(p1.fp) != 64 {
		t.Fatalf("fingerprint should be 64 hex chars, got %d", len(p1.fp))
	}
}

func TestPKIRefusesHalfCA(t *testing.T) {
	dir := t.TempDir()
	mustPKI(t, dir)
	if err := os.Remove(filepath.Join(dir, "ca.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := newPKI(dir); err == nil {
		t.Fatal("a CA without its key must be rejected instead of silently regenerated")
	}
}

func TestJoinTokenLifecycle(t *testing.T) {
	p := mustPKI(t, t.TempDir())

	tok, pl, err := p.newJoinToken(10 * time.Minute)
	if err != nil {
		t.Fatalf("newJoinToken: %v", err)
	}
	got, err := p.verifyJoinToken(tok)
	if err != nil {
		t.Fatalf("verifyJoinToken: %v", err)
	}
	if got.Nonce != pl.Nonce || got.Exp != pl.Exp {
		t.Fatalf("payload mismatch: %+v vs %+v", got, pl)
	}

	// 改 payload 里的一个字节就必须失败。
	// 注意不能改字符串末尾：base64 最后一个字符有几位不参与解码，
	// 改它可能解出完全相同的字节，那测的就不是签名了。
	parts := strings.Split(tok, ".")
	body, err := b64urlDecode(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	body[0] ^= 0x01
	tampered := "wb1." + b64url(body) + "." + parts[2]
	if _, err := p.verifyJoinToken(tampered); err == nil {
		t.Fatal("tampered token must be rejected")
	}

	// 另一个主控的密钥签的令牌不能通用
	other := mustPKI(t, t.TempDir())
	if _, err := other.verifyJoinToken(tok); err == nil {
		t.Fatal("a token signed by another CA must be rejected")
	}

	// 过期令牌
	expiredBody, err := json.Marshal(joinTokenPayload{
		V: distProtoVersion, Nonce: randomHex(8),
		Iat: time.Now().Add(-time.Hour).Unix(), Exp: time.Now().Add(-time.Minute).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	expired := "wb1." + b64url(expiredBody) + "." + b64url(p.mac(expiredBody))
	if _, err := p.verifyJoinToken(expired); err == nil {
		t.Fatal("expired token must be rejected")
	}

	// 版本不一致
	wrongVerBody, err := json.Marshal(joinTokenPayload{
		V: distProtoVersion + 1, Nonce: randomHex(8),
		Iat: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongVer := "wb1." + b64url(wrongVerBody) + "." + b64url(p.mac(wrongVerBody))
	if _, err := p.verifyJoinToken(wrongVer); err == nil {
		t.Fatal("token with a different protocol version must be rejected")
	}
}

func TestIssueClientCert(t *testing.T) {
	p := mustPKI(t, t.TempDir())
	key, csr, err := newAgentCSR("loadgen-1")
	if err != nil {
		t.Fatal(err)
	}
	certPEM, serial, err := p.issueClientCert(csr, "node-12345678")
	if err != nil {
		t.Fatalf("issueClientCert: %v", err)
	}
	leaf, err := parseCertPEM(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "node-12345678" {
		t.Fatalf("CN should be the master assigned id, got %q", leaf.Subject.CommonName)
	}
	if serial == "" {
		t.Fatal("serial should not be empty")
	}
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		t.Fatal("issued cert must match the CSR public key")
	}
	foundClientAuth := false
	for _, u := range leaf.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth {
			foundClientAuth = true
		}
	}
	if !foundClientAuth {
		t.Fatal("agent cert must be restricted to client auth")
	}
	if leaf.NotAfter.Before(time.Now().Add(distAgentCertTTL - time.Hour)) {
		t.Fatalf("agent cert lifetime too short: %s", leaf.NotAfter)
	}
}

func TestIssueClientCertRejectsWeakKey(t *testing.T) {
	p := mustPKI(t, t.TempDir())
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, weak)
	if err != nil {
		t.Fatal(err)
	}
	csr := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	if _, _, err := p.issueClientCert(csr, "node-deadbeef"); err == nil {
		t.Fatal("1024 bit RSA must be rejected")
	}
}

func TestVerifyPinnedChain(t *testing.T) {
	p := mustPKI(t, t.TempDir())
	cert, err := p.issueServerCert([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.Certificate) < 2 {
		t.Fatalf("server cert should ship the CA in the chain, got %d certs", len(cert.Certificate))
	}

	if err := verifyPinnedChain(cert.Certificate, p.fp); err != nil {
		t.Fatalf("pinned chain should verify: %v", err)
	}
	if err := verifyPinnedChain(cert.Certificate, strings.Repeat("0", 64)); err == nil {
		t.Fatal("wrong fingerprint must fail")
	}
	if err := verifyPinnedChain(cert.Certificate, groupFingerprint(p.fp)); err != nil {
		t.Fatalf("colon separated fingerprint should be accepted: %v", err)
	}
	if err := verifyPinnedChain(cert.Certificate[:1], p.fp); err == nil {
		t.Fatal("a leaf-only chain must not be accepted as its own trust anchor")
	}

	// 另一个主控的证书链不能用自己的指纹通过
	other := mustPKI(t, t.TempDir())
	otherCert, err := other.issueServerCert(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPinnedChain(otherCert.Certificate, p.fp); err == nil {
		t.Fatal("a chain from another CA must not verify against this fingerprint")
	}
}

func TestAdminPassword(t *testing.T) {
	// 数据目录不存在时也要能初始化：主控首次启动就是这种情况。
	dir := filepath.Join(t.TempDir(), "nested", "master")
	hash, generated, err := loadOrCreateAdminHash(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) != distGeneratedPwLen {
		t.Fatalf("generated password should be %d chars, got %d", distGeneratedPwLen, len(generated))
	}
	if !checkAdminPassword(hash, generated) {
		t.Fatal("generated password must verify")
	}
	if checkAdminPassword(hash, generated+"x") {
		t.Fatal("wrong password must not verify")
	}

	// 重启后沿用同一份哈希
	again, generated2, err := loadOrCreateAdminHash(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if generated2 != "" {
		t.Fatal("existing password must not be regenerated")
	}
	if !checkAdminPassword(again, generated) {
		t.Fatal("stored hash must still verify after restart")
	}

	// 显式指定密码：覆盖哈希且不回显
	hash3, gen3, err := loadOrCreateAdminHash(dir, "another-strong-pw")
	if err != nil {
		t.Fatal(err)
	}
	if gen3 != "" {
		t.Fatal("explicit password should not be echoed back")
	}
	if !checkAdminPassword(hash3, "another-strong-pw") {
		t.Fatal("explicit password must verify")
	}
	if checkAdminPassword(hash3, generated) {
		t.Fatal("old password must stop working after reset")
	}

	if _, _, err := loadOrCreateAdminHash(dir, "short"); err == nil {
		t.Fatal("short password must be rejected")
	}
}

func TestSessionStoreExpiry(t *testing.T) {
	store := newSessionStore(20 * time.Millisecond)
	token, csrf := store.create()
	if csrf == "" || token == "" {
		t.Fatal("session should have a token and a csrf value")
	}
	if _, ok := store.lookup(token); !ok {
		t.Fatal("fresh session should be valid")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := store.lookup(token); ok {
		t.Fatal("expired session must be rejected")
	}
	store.drop(token)
}

func TestRateLimiter(t *testing.T) {
	l := newRateLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("attempt %d should be allowed", i+1)
		}
	}
	if l.allow("1.2.3.4") {
		t.Fatal("4th attempt within the window must be rejected")
	}
	if !l.allow("5.6.7.8") {
		t.Fatal("another source must not be affected")
	}

	var blocked time.Duration
	for i := 0; i < distMaxLoginFails; i++ {
		blocked = l.fail("9.9.9.9")
	}
	if blocked <= 0 {
		t.Fatal("reaching the failure limit must block the source")
	}
	if d := l.blocked("9.9.9.9"); d <= 0 {
		t.Fatal("blocked source should report a wait time")
	}
	l.clear("9.9.9.9")
	if d := l.blocked("9.9.9.9"); d != 0 {
		t.Fatal("clear should release the block")
	}
}
