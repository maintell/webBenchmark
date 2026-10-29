package main

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// SSH 自动安装：主控直接登录压测机，把 agent 二进制丢上去并拉起。
//
// 安全上要交代清楚的三点：
//   1. 主控因此会短暂持有目标机的 SSH 凭据（只在内存里，不落盘）；
//   2. 主机密钥采用 TOFU：第一次必须把指纹回显给操作者确认，确认后记进
//      <data>/ssh_known_hosts，之后指纹变了就拒绝连接；
//   3. 上传的二进制就是主控自己的可执行文件（或 -agent-binary 指定的文件），
//      不存在"从网络下载一个 exe 再执行"的环节。

type sshInstallRequest struct {
	Host              string `json:"host"`
	Port              int    `json:"port"`
	User              string `json:"user"`
	Password          string `json:"password,omitempty"`
	KeyPath           string `json:"keyPath,omitempty"`
	KeyPEM            string `json:"keyPem,omitempty"`
	NodeName          string `json:"nodeName,omitempty"`
	MasterURL         string `json:"masterUrl"`
	Dir               string `json:"dir,omitempty"`
	AcceptFingerprint string `json:"acceptFingerprint,omitempty"`
	Insecure          bool   `json:"insecure,omitempty"`
}

type sshInstallResult struct {
	Host        string `json:"host"`
	OK          bool   `json:"ok"`
	Message     string `json:"message"`
	NodeID      string `json:"nodeId,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	NeedsTrust  bool   `json:"needsTrust,omitempty"`
}

// sshFingerprintError 用来把"没见过这个主机密钥"与其它错误区分开，
// 让控制台能弹出确认框，而不是只给一句失败原因。
type sshFingerprintError struct {
	fingerprint string
}

func (e *sshFingerprintError) Error() string {
	return "首次连接该主机，需要确认主机指纹"
}

var sshSafePath = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
var sshSafeName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

func (m *master) knownHostsPath() string { return filepath.Join(m.dir, "ssh_known_hosts") }

func (m *master) lookupHostKey(hostport string) string {
	data, err := os.ReadFile(m.knownHostsPath())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == hostport {
			return fields[1]
		}
	}
	return ""
}

func (m *master) rememberHostKey(hostport, fingerprint string) {
	line := hostport + " " + fingerprint + "\n"
	f, err := os.OpenFile(m.knownHostsPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if m.lookupHostKey(hostport) == "" {
		_, _ = f.WriteString(line)
	}
}

// hostKeyCallback 实现 TOFU：已知就严格比对，未知则要求操作者带指纹确认。
func (m *master) hostKeyCallback(hostport, accept string, insecure bool) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		fp := ssh.FingerprintSHA256(key)
		known := m.lookupHostKey(hostport)
		switch {
		case known == fp:
			return nil
		case known != "":
			return fmt.Errorf("主机密钥与记录不符（记录 %s，实际 %s），已拒绝连接", known, fp)
		case insecure:
			m.rememberHostKey(hostport, fp)
			return nil
		case accept != "" && accept == fp:
			m.rememberHostKey(hostport, fp)
			return nil
		default:
			return &sshFingerprintError{fingerprint: fp}
		}
	}
}

func (req *sshInstallRequest) normalize() error {
	req.Host = strings.TrimSpace(req.Host)
	if req.Host == "" {
		return errors.New("缺少目标主机")
	}
	if strings.ContainsAny(req.Host, " \t/") {
		return fmt.Errorf("主机地址不合法：%q", req.Host)
	}
	if req.Port == 0 {
		req.Port = 22
	}
	if req.Port < 1 || req.Port > 65535 {
		return fmt.Errorf("端口不合法：%d", req.Port)
	}
	req.User = strings.TrimSpace(req.User)
	if req.User == "" {
		req.User = "root"
	}
	if !sshSafeName.MatchString(req.User) {
		return fmt.Errorf("用户名不合法：%q", req.User)
	}
	req.MasterURL = strings.TrimSuffix(strings.TrimSpace(req.MasterURL), "/")
	if !strings.HasPrefix(req.MasterURL, "https://") && !strings.HasPrefix(req.MasterURL, "http://") {
		return errors.New("主控地址必须以 http:// 或 https:// 开头")
	}
	if req.Dir == "" {
		req.Dir = "$HOME/.webBenchmark-agent"
	}
	// 只允许 $HOME 前缀 + 安全字符：这个值会直接拼进远程 shell 命令，
	// 放行空格、分号、引号就等于把命令注入的口子留着。
	dirTail := strings.TrimPrefix(req.Dir, "$HOME")
	if !sshSafePath.MatchString(dirTail) || strings.Contains(dirTail, "$") {
		return fmt.Errorf("安装目录只允许字母、数字与 . _ - /（可用 $HOME 前缀），收到 %q", req.Dir)
	}
	if req.NodeName == "" {
		req.NodeName = "host"
	}
	req.NodeName = sanitizeNodeName(req.NodeName)
	if !sshSafeName.MatchString(req.NodeName) {
		return fmt.Errorf("节点名只允许字母、数字与 . _ -，收到 %q", req.NodeName)
	}
	if req.Password == "" && req.KeyPath == "" && req.KeyPEM == "" {
		return errors.New("需要 SSH 密码或私钥之一")
	}
	return nil
}

func (req *sshInstallRequest) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	if req.Password != "" {
		methods = append(methods, ssh.Password(req.Password))
	}
	keyPEM := req.KeyPEM
	if keyPEM == "" && req.KeyPath != "" {
		data, err := os.ReadFile(req.KeyPath)
		if err != nil {
			return nil, fmt.Errorf("读取私钥 %s: %w", req.KeyPath, err)
		}
		keyPEM = string(data)
	}
	if keyPEM != "" {
		signer, err := ssh.ParsePrivateKey([]byte(keyPEM))
		if err != nil {
			return nil, fmt.Errorf("解析 SSH 私钥: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if len(methods) == 0 {
		return nil, errors.New("没有可用的 SSH 认证方式")
	}
	return methods, nil
}

// dialSSH 建立连接；*sshFingerprintError 会原样返回给调用方做确认。
func (m *master) dialSSH(req sshInstallRequest) (*ssh.Client, string, error) {
	auths, err := req.authMethods()
	if err != nil {
		return nil, "", err
	}
	hostport := net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	cfg := &ssh.ClientConfig{
		User:            req.User,
		Auth:            auths,
		HostKeyCallback: m.hostKeyCallback(hostport, req.AcceptFingerprint, req.Insecure),
		Timeout:         15 * time.Second,
	}
	client, err := ssh.Dial("tcp", hostport, cfg)
	if err != nil {
		return nil, hostport, err
	}
	return client, hostport, nil
}

func (m *master) handleSSHProbe(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if _, ok := m.requireAdmin(w, r, true); !ok {
		return
	}
	var req sshInstallRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	if err := req.normalize(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	client, hostport, err := m.dialSSH(req)
	if err != nil {
		var fpErr *sshFingerprintError
		if errors.As(err, &fpErr) {
			writeJSON(w, http.StatusOK, map[string]any{
				"ok": true, "needsTrust": true, "fingerprint": fpErr.fingerprint,
			})
			return
		}
		writeError(w, http.StatusBadRequest, "连接失败："+err.Error())
		return
	}
	defer client.Close()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "needsTrust": false, "fingerprint": m.lookupHostKey(hostport),
	})
}

func (m *master) handleSSHInstall(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if _, ok := m.requireAdmin(w, r, true); !ok {
		return
	}
	var req sshInstallRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	if err := req.normalize(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res := m.installAgent(req)
	writeJSON(w, http.StatusOK, res)
}

// installAgent 完成"上传二进制 -> 拉起 -> 等待连回主控"三步。
func (m *master) installAgent(req sshInstallRequest) sshInstallResult {
	res := sshInstallResult{Host: req.Host}
	if req.Port != 22 {
		res.Host = net.JoinHostPort(req.Host, strconv.Itoa(req.Port))
	}

	client, hostport, err := m.dialSSH(req)
	if err != nil {
		var fpErr *sshFingerprintError
		if errors.As(err, &fpErr) {
			res.Message = "首次连接该主机，请确认主机指纹后再安装"
			res.Fingerprint = fpErr.fingerprint
			res.NeedsTrust = true
			return res
		}
		res.Message = "SSH 连接失败：" + err.Error()
		return res
	}
	defer client.Close()
	res.Fingerprint = m.lookupHostKey(hostport)

	uname, err := sshOutput(client, "uname -sm")
	if err != nil {
		res.Message = "目标机不是类 Unix 系统（uname 执行失败）"
		return res
	}
	if !platformMatches(uname) {
		res.Message = fmt.Sprintf("目标平台 %q 与上传的二进制（%s/%s）不匹配，请用 -agent-binary 指定对应平台的版本",
			uname, runtime.GOOS, runtime.GOARCH)
		return res
	}

	bin, err := m.agentBinary()
	if err != nil {
		res.Message = err.Error()
		return res
	}
	binPath := req.Dir + "/webBenchmark-agent"
	if err := sshUpload(client, req.Dir, binPath, bin); err != nil {
		res.Message = "上传二进制失败：" + err.Error()
		return res
	}

	// 每次安装都签发一枚新的一次性令牌，令牌只对这一台机器有效。
	join, err := m.newJoin(req.MasterURL)
	if err != nil {
		res.Message = "签发加入凭据失败：" + err.Error()
		return res
	}
	startCmd := fmt.Sprintf(
		"cd %s && nohup %s -join %s -node-name %s -data %s/data >> %s/agent.log 2>&1 & echo started",
		req.Dir, binPath, join.encode(), req.NodeName, req.Dir, req.Dir)
	if _, err := sshOutput(client, startCmd); err != nil {
		res.Message = "启动失败：" + err.Error()
		return res
	}

	nodeID, ok := m.waitForNode(req.NodeName, 15*time.Second)
	if !ok {
		res.Message = fmt.Sprintf("已在目标机启动，但 15 秒内没有连回来；请查看 %s/agent.log", req.Dir)
		return res
	}
	res.OK = true
	res.NodeID = nodeID
	res.Message = "安装完成，节点已连接"
	m.audit.event("ssh_install", map[string]any{
		"host": req.Host, "user": req.User, "node": nodeID, "name": req.NodeName,
	})
	return res
}

// agentBinary 返回要上传的二进制：默认就是主控自己（同一个程序两种角色）。
func (m *master) agentBinary() ([]byte, error) {
	path := strings.TrimSpace(*agentBinaryPath)
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("定位主控可执行文件失败：%w", err)
		}
		path = exe
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取待上传的二进制 %s: %w", path, err)
	}
	return data, nil
}

func (m *master) waitForNode(name string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		for id, n := range m.nodes {
			if n.name == name && n.conn != nil && !n.revoked {
				m.mu.Unlock()
				return id, true
			}
		}
		m.mu.Unlock()
		time.Sleep(500 * time.Millisecond)
	}
	return "", false
}

func sshOutput(client *ssh.Client, cmd string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()
	var out, errBuf bytes.Buffer
	sess.Stdout = &out
	sess.Stderr = &errBuf
	_ = sess.Setenv("LC_ALL", "C")
	if err := sess.Run(cmd); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(shortText(msg, 300))
	}
	return strings.TrimSpace(out.String()), nil
}

func sshUpload(client *ssh.Client, dir, binPath string, bin []byte) error {
	if _, err := sshOutput(client, "mkdir -p "+dir); err != nil {
		return err
	}
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	var errBuf bytes.Buffer
	sess.Stderr = &errBuf
	sess.Stdin = bytes.NewReader(bin)
	if err := sess.Run("cat > " + binPath); err != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(shortText(msg, 300))
	}
	if _, err := sshOutput(client, "chmod 0755 "+binPath); err != nil {
		return err
	}
	return nil
}

// platformMatches 把 uname 的输出和本程序的编译目标对上。
// 对不上就提前报错，别等用户盯着 agent.log 猜。
func platformMatches(uname string) bool {
	fields := strings.Fields(strings.ToLower(uname))
	if len(fields) < 2 {
		return false
	}
	osName, arch := fields[0], fields[1]
	goos, goarch := runtime.GOOS, runtime.GOARCH
	if goos != osName {
		return false
	}
	switch goarch {
	case "amd64":
		return arch == "x86_64" || arch == "amd64"
	case "arm64":
		return arch == "aarch64" || arch == "arm64"
	case "arm":
		return strings.HasPrefix(arch, "armv")
	case "386":
		return arch == "i386" || arch == "i686"
	default:
		return arch == goarch
	}
}
