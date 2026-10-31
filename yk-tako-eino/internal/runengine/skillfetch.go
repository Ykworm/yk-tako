// skillfetch.go 是技能来源的抓取层（doc/52 §6.5 C2 拍）：只允许公开 http(s)，
// 出口侧把每个连接解析到的 IP 都验一遍。
//
// 为什么要在 DialContext 里验 IP 而不是只验 URL：URL 里的域名是用户给的，
// `http://evil.example/` 可以解析到 127.0.0.1 或 169.254.169.254。只查一层字符串
// 等于把「内网可达」交给 DNS 的玩笑。这里解析一次、验完直接连那个 IP（不再按域名
// 二次解析），DNS rebinding 也换不掉已经建立的连接。
//
// 重定向同样每跳走这个 DialContext（http.Transport 对每一跳都新建连接），
// 所以「302 到 10.0.0.1」也过不去；CheckRedirect 只额外限制跳数与协议。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// errSkillFetchBlocked 表示目标解析到了非公网地址（loopback/私网/链路本地/元数据）。
// 抓取层把它翻成 116109——这是用户给的来源不合规，不是网络故障。
var errSkillFetchBlocked = errors.New("目标地址不是公网地址")

// isPublicSkillIP 是 SSRF 的正面清单：只放行全球单播地址。
func isPublicSkillIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		if v4.IsLoopback() || v4.IsPrivate() || v4.IsLinkLocalUnicast() ||
			v4.IsLinkLocalMulticast() || v4.IsUnspecified() || v4.IsMulticast() {
			return false
		}
		// 100.64.0.0/10 是运营商级 NAT，云上常把内网服务放在这一段；Go 的 IsPrivate 不覆盖。
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return false
		}
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	return true
}

// skillFetchDial 在真正拨号前解析并验 IP，然后**直连验过的 IP**。
func skillFetchDial(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	if len(ips) == 0 {
		return nil, errSkillFetchBlocked
	}
	for _, ia := range ips {
		if !isPublicSkillIP(ia.IP) {
			return nil, fmt.Errorf("%w（%s → %s）", errSkillFetchBlocked, host, ia.IP)
		}
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	var lastErr error
	for _, ia := range ips {
		conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ia.IP.String(), port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, lastErr
}

// newSkillFetchClient 造抓取用的 HTTP 客户端。
//
// Proxy 置 nil：环境变量里的 HTTP_PROXY 会把流量交给代理进程去解析域名，
// 我们这一层的 IP 检查就失效了。技能抓取只走直连。
func newSkillFetchClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           skillFetchDial,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
			ForceAttemptHTTP2:     true,
		},
		Timeout: 90 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("重定向超过 5 跳")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("重定向到非 http(s) 地址")
			}
			return nil
		},
	}
}

// getSkillURL 取一个地址并返回 body（≤maxBytes）。非 200 与网络错误分开报：
// 401/403/404 是「来源不存在或不是公开仓库」（116109），其余是抓取故障（116110）。
func getSkillURL(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, sourceErr("来源链接不合法")
	}
	req.Header.Set("User-Agent", "tako-skill-resolver/1")
	req.Header.Set("Accept", "application/octet-stream, application/vnd.github+json")
	resp, err := newSkillFetchClient().Do(req)
	if err != nil {
		if errors.Is(err, errSkillFetchBlocked) {
			return nil, sourceErr("来源地址不是公网地址（拒绝内网/回环/元数据地址）")
		}
		return nil, skillWriteError(http.StatusBadGateway, codeSkillFetchFailed,
			"抓取来源失败："+err.Error())
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return nil, sourceErr(fmt.Sprintf("来源不存在或不是公开仓库（HTTP %d）", resp.StatusCode))
	case http.StatusTooManyRequests:
		return nil, skillWriteError(http.StatusBadGateway, codeSkillFetchFailed,
			"来源方限流（HTTP 429），稍后再试")
	default:
		return nil, skillWriteError(http.StatusBadGateway, codeSkillFetchFailed,
			fmt.Sprintf("抓取来源失败（HTTP %d）", resp.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, skillWriteError(http.StatusBadGateway, codeSkillFetchFailed,
			"读取来源失败："+err.Error())
	}
	if int64(len(data)) > maxBytes {
		return nil, skillWriteError(http.StatusRequestEntityTooLarge, codeSkillPackageTooLarge,
			fmt.Sprintf("来源归档超过下载上限（%d 字节）", maxBytes))
	}
	return data, nil
}

// escapePathSegments 按段转义 ref/分支名（分支可以带 `/`，不能整段 QueryEscape）。
func escapePathSegments(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// skillArchiveURL 把一个来源规格翻成「下载什么」。
//
// GitHub 直接打 codeload：`github.com/<o>/<r>/archive/…` 只是一层 302，真正的
// 字节由 codeload 提供；直连 codeload 少一跳（也少一个可能打不开的域名）。
// 不调 API：`/tar.gz/HEAD` 走默认分支，不吃 rate limit。
// GitLab 必须问一次 default_branch（它的归档 URL 要具体分支名），
// 这一步也走同一个受限客户端。
func skillArchiveURL(ctx context.Context, spec skillSourceSpec) (string, error) {
	ref := spec.Ref
	switch spec.Kind {
	case "archive":
		return spec.URL, nil
	case "github":
		if ref == "" {
			ref = "HEAD"
		}
		return fmt.Sprintf("https://codeload.github.com/%s/%s/tar.gz/%s",
			url.PathEscape(spec.Owner), url.PathEscape(spec.Repo), escapePathSegments(ref)), nil
	case "gitlab":
		ns := escapePathSegments(spec.Namespace)
		if ref == "" {
			branch, err := gitlabDefaultBranch(ctx, spec.Namespace, spec.Repo)
			if err != nil {
				return "", err
			}
			ref = branch
		}
		repo := url.PathEscape(spec.Repo)
		return fmt.Sprintf("https://gitlab.com/%s/-/archive/%s/%s-%s.tar.gz",
			ns, escapePathSegments(ref), repo, escapePathSegments(ref)), nil
	}
	return "", sourceErr("不认识的来源类型")
}

// gitlabDefaultBranch 问 GitLab 公开项目的默认分支。
//
// 只取一个字段；响应超过 1MB 视为异常（正常就几百字节）。
// 私有/不存在的项目在 HTTP 层回 401/403/404 → 116109。
func gitlabDefaultBranch(ctx context.Context, namespace, repo string) (string, error) {
	full := namespace + "/" + repo
	u := "https://gitlab.com/api/v4/projects/" + url.QueryEscape(full)
	data, err := getSkillURL(ctx, u, 1<<20)
	if err != nil {
		return "", err
	}
	var body struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return "", skillWriteError(http.StatusBadGateway, codeSkillFetchFailed,
			"GitLab 项目信息解析失败")
	}
	if strings.TrimSpace(body.DefaultBranch) == "" {
		return "", sourceErr("GitLab 项目没有默认分支（空仓库？）")
	}
	return body.DefaultBranch, nil
}
