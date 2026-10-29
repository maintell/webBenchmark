package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// distServerName 是主控证书里的固定身份名。
	// 从节点靠"CA 指纹 + 这条固定身份"确认对端就是自己的主控，
	// 而不是靠 IP / DNS：压测机常用裸 IP 直连，主机名验证在那种部署里
	// 只会变成"证书是给另一个主机名签的"这类误报。
	distServerName = "webbenchmark-master"

	distAgentCertTTL = 90 * 24 * time.Hour
	distRenewBefore  = 30 * 24 * time.Hour
	distCACertTTL    = 10 * 365 * 24 * time.Hour
)

// pki 是主控的信任根：自签 CA、签发叶证书的能力、以及令牌 / 会话的 HMAC 密钥。
//
// 为什么不用系统证书或 Let's Encrypt：压测机通常在隔离网段里用裸 IP 互连，
// 拿不到公网域名证书；而自建 CA 天然满足"只有我签发的节点才能进集群"
// 这个比域名验证更强的条件。
type pki struct {
	dir     string
	cert    *x509.Certificate
	certPEM []byte
	key     crypto.Signer
	secret  []byte
	fp      string // CA 证书的 SHA-256，小写十六进制，不带分隔符
}

// newPKI 打开（或首次创建）数据目录里的信任材料。
// 已有的 CA 绝不覆盖：一旦重新生成，所有已入网节点都会因为指纹不符而失联。
func newPKI(dir string) (*pki, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录 %s: %w", dir, err)
	}
	cert, signer, certPEM, err := loadOrCreateCA(dir)
	if err != nil {
		return nil, err
	}
	secret, err := loadOrCreateSecret(filepath.Join(dir, "secret.key"))
	if err != nil {
		return nil, err
	}
	return &pki{
		dir:     dir,
		cert:    cert,
		certPEM: certPEM,
		key:     signer,
		secret:  secret,
		fp:      sha256Hex(cert.Raw),
	}, nil
}

// displayFingerprint 返回人类可读的指纹：按字节分组，方便电话或聊天里核对。
func (p *pki) displayFingerprint() string {
	return groupFingerprint(p.fp)
}

func groupFingerprint(fp string) string {
	if len(fp)%2 != 0 {
		return fp
	}
	parts := make([]string, 0, len(fp)/2)
	for i := 0; i < len(fp); i += 2 {
		parts = append(parts, fp[i:i+2])
	}
	return strings.Join(parts, ":")
}

// normalizeFingerprint 接受 "AB:CD:…"、"abcd…" 等各种写法，统一成小写无分隔。
func normalizeFingerprint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimPrefix(s, "sha256:")
	s = strings.ReplaceAll(s, ":", "")
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ReplaceAll(s, "-", "")
	return s
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// loadOrCreateCA 读取或生成自签 CA。
// 只存在一半文件时直接报错而不是"补齐"：那意味着有人在手工改动信任根，
// 静默重建会把整个集群的入网关系弄坏。
func loadOrCreateCA(dir string) (*x509.Certificate, crypto.Signer, []byte, error) {
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	certExists := fileExists(certPath)
	keyExists := fileExists(keyPath)
	switch {
	case certExists && keyExists:
		certPEM, err := os.ReadFile(certPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("读取 %s: %w", certPath, err)
		}
		cert, err := parseCertPEM(certPEM)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("解析 %s: %w", certPath, err)
		}
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("读取 %s: %w", keyPath, err)
		}
		signer, err := parseKeyPEM(keyPEM)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("解析 %s: %w", keyPath, err)
		}
		if !cert.IsCA {
			return nil, nil, nil, fmt.Errorf("%s 不是 CA 证书", certPath)
		}
		return cert, signer, certPEM, nil
	case certExists != keyExists:
		return nil, nil, nil, fmt.Errorf(
			"%s 与 %s 只存在一个，拒绝自动重建 CA（重建会让所有已入网节点失联）；请补回缺失文件或清空数据目录后重新签发",
			certPath, keyPath)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("生成 CA 私钥: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: "webBenchmark master CA", Organization: []string{"webBenchmark"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(distCACertTTL),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("签发 CA 证书: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM, err := encodeKeyPEM(key)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := writeFileAtomic(certPath, certPEM, 0o600); err != nil {
		return nil, nil, nil, err
	}
	if err := writeFileAtomic(keyPath, keyPEM, 0o600); err != nil {
		return nil, nil, nil, err
	}
	return cert, key, certPEM, nil
}

// issueServerCert 签发控制台/控制面用的服务端证书。
//
// SAN 里始终包含固定的 distServerName，另外补上本机可能被访问到的名字与地址；
// 用户从外网用别的 IP 访问时可以用 -master-san 追加。
func (p *pki) issueServerCert(hosts []string) (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: distServerName, Organization: []string{"webBenchmark"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(1, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{distServerName},
	}
	seen := map[string]bool{distServerName: true}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] || h == "0.0.0.0" || h == "::" {
			continue
		}
		seen[h] = true
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.cert, key.Public(), p.key)
	if err != nil {
		return tls.Certificate{}, err
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	// 链里带上 CA：从节点首次入网时只握着指纹，需要从握手里拿到 CA 证书
	// 才能做"指纹吻合 + 叶子确实由该 CA 签发"的双重校验。
	chainPEM := append(append([]byte{}, leafPEM...), p.certPEM...)
	keyPEM, err := encodeKeyPEM(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	cert, err := tls.X509KeyPair(chainPEM, keyPEM)
	if err != nil {
		return tls.Certificate{}, err
	}
	// 落一份到数据目录便于排查与浏览器导入；启动时总是重签，
	// 因为 SAN 会随 -master 的监听地址变化。
	_ = writeFileAtomic(filepath.Join(p.dir, "master.crt"), chainPEM, 0o600)
	_ = writeFileAtomic(filepath.Join(p.dir, "master.key"), keyPEM, 0o600)
	return cert, nil
}

// issueClientCert 用节点的 CSR 签发客户端证书。
// 身份（CN）始终由主控决定，不复用 CSR 里的 CN —— 否则入网节点可以自称任意名字。
func (p *pki) issueClientCert(csrPEM string, nodeID string) (certPEM []byte, serial string, err error) {
	csr, err := parseCSR(csrPEM)
	if err != nil {
		return nil, "", err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, "", fmt.Errorf("CSR 签名无效: %w", err)
	}
	switch key := csr.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if key.Curve != elliptic.P256() && key.Curve != elliptic.P384() {
			return nil, "", errors.New("CSR 只接受 P-256 / P-384 的 ECDSA 公钥")
		}
	case *rsa.PublicKey:
		if key.N.BitLen() < 2048 {
			return nil, "", errors.New("CSR 的 RSA 公钥至少 2048 位")
		}
	default:
		return nil, "", fmt.Errorf("不支持的 CSR 公钥类型 %T", csr.PublicKey)
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          newSerial(),
		Subject:               pkix.Name{CommonName: nodeID, Organization: []string{"webBenchmark agent"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(distAgentCertTTL),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.cert, csr.PublicKey, p.key)
	if err != nil {
		return nil, "", fmt.Errorf("签发节点证书: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), tmpl.SerialNumber.Text(16), nil
}

// caPool 返回只信任本 CA 的证书池，用于校验节点客户端证书。
func (p *pki) caPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(p.cert)
	return pool
}

// joinTokenPayload 是一次性加入令牌的内容。
type joinTokenPayload struct {
	V     int    `json:"v"`
	Nonce string `json:"n"`
	Iat   int64  `json:"i"`
	Exp   int64  `json:"e"`
}

// newJoinToken 生成一次性加入令牌。
//
// 令牌不落库，只把用掉的 nonce 记下来防重放；因此主控重启后
// 已过期的令牌自然失效，未过期的仍然可用（对运维更友好）。
func (p *pki) newJoinToken(ttl time.Duration) (string, joinTokenPayload, error) {
	if ttl <= 0 {
		return "", joinTokenPayload{}, errors.New("加入令牌有效期必须大于 0")
	}
	now := time.Now()
	pl := joinTokenPayload{
		V:     distProtoVersion,
		Nonce: randomHex(16),
		Iat:   now.Unix(),
		Exp:   now.Add(ttl).Unix(),
	}
	body, err := json.Marshal(pl)
	if err != nil {
		return "", joinTokenPayload{}, err
	}
	return "wb1." + b64url(body) + "." + b64url(p.mac(body)), pl, nil
}

// verifyJoinToken 校验令牌格式、签名与有效期。
// 单次使用由调用方在持锁状态下检查 nonce，避免两个并发请求同时通过。
func (p *pki) verifyJoinToken(tok string) (joinTokenPayload, error) {
	var pl joinTokenPayload
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) != 3 || parts[0] != "wb1" {
		return pl, errors.New("加入令牌格式不正确")
	}
	body, err := b64urlDecode(parts[1])
	if err != nil {
		return pl, errors.New("加入令牌内容损坏")
	}
	sig, err := b64urlDecode(parts[2])
	if err != nil {
		return pl, errors.New("加入令牌签名损坏")
	}
	if !hmac.Equal(sig, p.mac(body)) {
		return pl, errors.New("加入令牌签名不匹配")
	}
	if err := json.Unmarshal(body, &pl); err != nil {
		return pl, errors.New("加入令牌内容损坏")
	}
	if pl.V != distProtoVersion {
		return pl, fmt.Errorf("加入令牌版本 %d 与控制面版本 %d 不一致", pl.V, distProtoVersion)
	}
	if pl.Nonce == "" {
		return pl, errors.New("加入令牌缺少 nonce")
	}
	if time.Now().Unix() > pl.Exp {
		return pl, errors.New("加入令牌已过期，请在控制台重新签发")
	}
	return pl, nil
}

func (p *pki) mac(body []byte) []byte {
	m := hmac.New(sha256.New, p.secret)
	m.Write(body)
	return m.Sum(nil)
}

// verifyPinnedChain 是从节点侧的核心校验：不依赖系统根证书，
// 只承认"链根的 SHA-256 等于我手里这把指纹"的主控证书。
//
// rawCerts 是 TLS 层收到的证书链（叶子在前）。首次入网时从节点还没拿到 CA，
// 所以要先用指纹确认链根，再用链根验证叶子确实由它签发——
// 这两步合起来才等价于"证书固定"，少任何一步都能被中间人绕过。
func verifyPinnedChain(rawCerts [][]byte, fingerprint string) error {
	fp := normalizeFingerprint(fingerprint)
	if len(fp) != 64 {
		return errors.New("CA 指纹必须是 64 位十六进制")
	}
	if len(rawCerts) == 0 {
		return errors.New("对端没有出示证书")
	}
	certs := make([]*x509.Certificate, 0, len(rawCerts))
	for _, raw := range rawCerts {
		c, err := x509.ParseCertificate(raw)
		if err != nil {
			return fmt.Errorf("解析对端证书: %w", err)
		}
		certs = append(certs, c)
	}

	root := certs[len(certs)-1]
	if got := sha256Hex(root.Raw); got != fp {
		return fmt.Errorf("CA 指纹不匹配：期望 %s，实际 %s", groupFingerprint(fp), groupFingerprint(got))
	}
	// 只有一张证书时它必须就是那张 CA，否则叶子既当根又当叶，锚点就没意义了。
	if len(certs) == 1 && !root.IsCA {
		return errors.New("对端只出示了叶子证书，无法用指纹锚定信任根")
	}

	roots := x509.NewCertPool()
	roots.AddCert(root)
	inter := x509.NewCertPool()
	for _, c := range certs[1 : len(certs)-1] {
		inter.AddCert(c)
	}
	if _, err := certs[0].Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		DNSName:       distServerName,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return fmt.Errorf("主控证书链校验失败: %w", err)
	}
	return nil
}

func loadOrCreateSecret(path string) ([]byte, error) {
	if fileExists(path) {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取 %s: %w", path, err)
		}
		raw, err := b64urlDecode(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("%s 内容不是 32 字节密钥", path)
		}
		return raw, nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(path, []byte(b64url(secret)), 0o600); err != nil {
		return nil, err
	}
	return secret, nil
}

func newSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return n.Add(n, big.NewInt(1))
}

func encodeKeyPEM(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func parseKeyPEM(data []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("不是 PEM 编码的私钥")
	}
	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if signer, ok := key.(crypto.Signer); ok {
			return signer, nil
		}
		return nil, fmt.Errorf("私钥类型 %T 不支持签名", key)
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	return nil, errors.New("无法解析私钥，只支持 PKCS#8 或 EC 私钥")
}

func parseCertPEM(data []byte) (*x509.Certificate, error) {
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil, errors.New("没有找到 CERTIFICATE 块")
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		return x509.ParseCertificate(block.Bytes)
	}
}

func parseCSR(csrPEM string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(csrPEM)))
	if block == nil {
		return nil, errors.New("CSR 不是 PEM 编码")
	}
	if block.Type != "CERTIFICATE REQUEST" && block.Type != "NEW CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("CSR 的 PEM 类型是 %q", block.Type)
	}
	return x509.ParseCertificateRequest(block.Bytes)
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func b64urlDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeFileAtomic 先写临时文件再改名，避免进程被杀时留下半个证书/半个状态文件。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
