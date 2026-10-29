package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// joinInfo 是"加入集群"所需的全部信息。
// 它把地址、一次性令牌、CA 指纹打包成一段可直接复制的字符串，
// 这样运维不需要在从节点上分别粘三个值、也不容易漏掉指纹。
type joinInfo struct {
	V           int       `json:"v"`
	URL         string    `json:"url"`
	Token       string    `json:"token"`
	Fingerprint string    `json:"fingerprint"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

func (j joinInfo) encode() string {
	data, err := json.Marshal(j)
	if err != nil {
		return ""
	}
	return "wb1_" + b64url(data)
}

// decodeJoinString 容忍三种常见粘贴方式：裸 wb1_ 串、带前缀的命令行、
// 以及在聊天窗口里被软换行拆开的整行命令。
func decodeJoinString(s string) (joinInfo, error) {
	var info joinInfo
	s = strings.TrimSpace(s)
	if s == "" {
		return info, errors.New("加入凭据为空")
	}
	if fields := strings.Fields(s); len(fields) > 1 {
		s = fields[len(fields)-1]
	}
	s = strings.TrimPrefix(s, "wb1_")
	data, err := b64urlDecode(s)
	if err != nil {
		return info, errors.New("加入凭据已损坏，请重新复制（注意不要带上多余字符）")
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return info, errors.New("加入凭据内容无法解析")
	}
	if info.V != distProtoVersion {
		return info, fmt.Errorf("加入凭据版本 %d 与当前版本 %d 不一致，请让主从使用同一版本", info.V, distProtoVersion)
	}
	if !strings.HasPrefix(info.URL, "https://") {
		return info, errors.New("加入凭据里的主控地址不是 https")
	}
	if len(normalizeFingerprint(info.Fingerprint)) != 64 {
		return info, errors.New("加入凭据里的 CA 指纹不合法")
	}
	return info, nil
}

// agentModeRequested 判断本次是否以从节点身份启动。
func agentModeRequested() bool {
	return *agentFlag || strings.TrimSpace(*joinString) != "" || strings.TrimSpace(*masterURL) != ""
}

// validateDistFlags 把模式冲突挡在启动阶段：
// 一个进程不可能同时是主控、从节点和本机压测器。
func validateDistFlags() error {
	if *masterListen != "" && agentModeRequested() {
		return errors.New("-master 与 -agent/-join 不能同时使用")
	}
	if *joinTTLMin < 5 || *joinTTLMin > 1440 {
		return fmt.Errorf("-join-ttl 必须在 5..1440 分钟之间，收到 %d", *joinTTLMin)
	}
	if fp := normalizeFingerprint(*caFingerprint); fp != "" && len(fp) != 64 {
		return fmt.Errorf("-ca-fingerprint 必须是 64 位十六进制（可带冒号），收到 %q", *caFingerprint)
	}
	if *masterListen != "" && (*targetInput != "" || *subscribe != "") {
		return errors.New("-master 模式不跑本机压测，去掉 -s / -sub")
	}
	if agentModeRequested() && (*targetInput != "" || *subscribe != "") {
		return errors.New("从节点模式不接受 -s / -sub，任务由主控下发")
	}
	return nil
}

// masterHosts 收集服务端证书要覆盖的名字。
// 监听 0.0.0.0 时无法知道运维会从哪个地址访问，用主机名兜底，
// 其余由 -master-san 补充。
func masterHosts(bind string) []string {
	hosts := []string{"127.0.0.1", "::1", "localhost"}
	if hn, err := os.Hostname(); err == nil && hn != "" {
		hosts = append(hosts, hn)
	}
	if h, _, err := net.SplitHostPort(bind); err == nil {
		if h != "" && h != "0.0.0.0" && h != "::" {
			hosts = append(hosts, h)
		}
	}
	for _, extra := range strings.Split(*masterSAN, ",") {
		if s := strings.TrimSpace(extra); s != "" {
			hosts = append(hosts, s)
		}
	}
	return hosts
}

func runMasterMode() error {
	dir := strings.TrimSpace(*distData)
	if dir == "" {
		dir = "webBenchmark-master"
	}
	hash, generated, err := loadOrCreateAdminHash(dir, *distAdminPassword)
	if err != nil {
		return err
	}
	m, err := newMaster(masterOptions{
		Dir:       dir,
		Hosts:     masterHosts(*masterListen),
		JoinTTL:   time.Duration(*joinTTLMin) * time.Minute,
		AdminHash: hash,
	})
	if err != nil {
		return err
	}
	defer m.close()

	// 先绑定端口再打印横幅：端口被占时不应该先给出一串看起来可用的地址。
	ln, err := net.Listen("tcp", *masterListen)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", *masterListen, err)
	}

	publicURL := masterPublicURL(*masterListen)
	info, err := m.newJoin(publicURL)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, `webBenchmark %s 主控
  控制台     %s/
  CA 指纹    sha256:%s
  加入命令   webBenchmark -join %s

  从节点只需对外发起连接，不需要开放任何入站端口。
`, version, publicURL, m.pki.displayFingerprint(), info.encode())
	if generated != "" {
		fmt.Fprintf(os.Stderr, "  控制台密码 %s\n"+
			"             （首次启动自动生成，只显示这一次；忘记后用 -admin-password 重置）\n", generated)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return m.serve(ctx, ln)
}

func runAgentMode() error {
	info, err := resolveJoinInfo()
	if err != nil {
		return err
	}
	dir := strings.TrimSpace(*distData)
	if dir == "" {
		dir = "webBenchmark-agent"
	}
	name := strings.TrimSpace(*nodeName)
	if name == "" {
		if hn, err := os.Hostname(); err == nil {
			name = hn
		}
	}
	a, err := newAgent(agentConfig{
		Dir:         dir,
		URL:         info.URL,
		Name:        name,
		Token:       info.Token,
		Fingerprint: info.Fingerprint,
		CAFile:      *caCertFile,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.run(ctx)
}

// resolveJoinInfo 汇总三种信息来源的优先级：
// 完整的 -join 凭据 > 拆开的 -master-url / -join-token / -ca-fingerprint。
// 已经入网过的数据目录允许全部留空，此时地址与指纹都来自本地状态。
func resolveJoinInfo() (joinInfo, error) {
	info := joinInfo{V: distProtoVersion}
	if s := strings.TrimSpace(*joinString); s != "" {
		decoded, err := decodeJoinString(s)
		if err != nil {
			return info, err
		}
		info = decoded
	}
	if v := strings.TrimSpace(*masterURL); v != "" {
		info.URL = strings.TrimSuffix(v, "/")
	}
	if v := strings.TrimSpace(*joinToken); v != "" {
		info.Token = v
	}
	if v := normalizeFingerprint(*caFingerprint); v != "" {
		info.Fingerprint = v
	}
	return info, nil
}
