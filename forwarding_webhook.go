package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

func validateForwardHeaders(headers []ForwardHeader) ([]ForwardHeader, error) {
	if len(headers) > 20 {
		return nil, errors.New("最多配置20个自定义 Header")
	}
	seen := map[string]bool{}
	out := make([]ForwardHeader, 0, len(headers))
	total := 0
	for _, h := range headers {
		h.Name = strings.TrimSpace(h.Name)
		if len(h.Name) == 0 || len(h.Name) > 128 || len(h.Value) > 4096 {
			return nil, errors.New("Header 名称为1–128字节，值最多4096字节")
		}
		for _, c := range h.Name {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
				return nil, errors.New("Header 名称含无效字符")
			}
		}
		for _, c := range h.Value {
			if (c < 32 && c != '\t') || c == 127 {
				return nil, errors.New("Header 值不能包含换行或控制字符")
			}
		}
		name := http.CanonicalHeaderKey(h.Name)
		switch name {
		case "Host", "Content-Length", "Transfer-Encoding", "Connection", "Proxy-Connection", "Keep-Alive", "Te", "Trailer", "Upgrade", "Content-Type", "Idempotency-Key", "X-Ec20-Event-Id":
			return nil, errors.New("连接控制、JSON 类型和去重 Header 由系统生成，不可覆盖")
		}
		if strings.HasPrefix(strings.ToLower(name), "proxy-") {
			return nil, errors.New("不支持代理控制 Header")
		}
		if seen[name] {
			return nil, errors.New("Header 名称重复（不区分大小写）")
		}
		seen[name] = true
		total += len(name) + len(h.Value)
		if total > 16384 {
			return nil, errors.New("自定义 Header 总大小最多16KB")
		}
		out = append(out, ForwardHeader{Name: name, Value: h.Value})
	}
	return out, nil
}

func validateWebhookURL(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || len(raw) > 2048 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("Webhook 必须是无内嵌用户名密码的 HTTP/HTTPS 地址")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return errors.New("Webhook 不允许本机或本地服务发现地址")
	}
	if ip, e := netip.ParseAddr(host); e == nil && !webhookIPAllowed(ip) {
		return errors.New("Webhook 不允许回环、链路本地或元数据地址")
	}
	if u.Scheme == "http" {
		ip, e := netip.ParseAddr(host)
		if e != nil || !ip.IsPrivate() {
			return errors.New("明文 HTTP 仅限显式填写的私网 IP；其他目标请使用 HTTPS")
		}
	}
	return nil
}
func webhookIPAllowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && !ip.IsUnspecified() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && ip.IsGlobalUnicast() && ip.String() != "100.100.100.200" && ip.String() != "168.63.129.16"
}
func maskedWebhook(raw string) string {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" {
		return "未配置"
	}
	return u.Scheme + "://" + u.Host + "/…"
}

// Linux procfs exposes local addresses without granting AF_NETLINK or network-control capabilities.
func localWebhookIPs() map[netip.Addr]bool {
	out := map[netip.Addr]bool{}
	if b, e := os.ReadFile("/proc/net/fib_trie"); e == nil {
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			if i > 0 && strings.Contains(line, "/32 host LOCAL") {
				p := strings.Fields(lines[i-1])
				if len(p) > 0 {
					if ip, e := netip.ParseAddr(p[len(p)-1]); e == nil {
						out[ip.Unmap()] = true
					}
				}
			}
		}
	}
	if b, e := os.ReadFile("/proc/net/if_inet6"); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			p := strings.Fields(line)
			if len(p) > 0 {
				if raw, e := hex.DecodeString(p[0]); e == nil && len(raw) == 16 {
					var bytes [16]byte
					copy(bytes[:], raw)
					out[netip.AddrFrom16(bytes).Unmap()] = true
				}
			}
		}
	}
	if len(out) == 0 {
		if all, e := net.InterfaceAddrs(); e == nil {
			for _, a := range all {
				if prefix, e := netip.ParsePrefix(a.String()); e == nil {
					out[prefix.Addr().Unmap()] = true
				}
			}
		}
	}
	return out
}
func (a *App) newWebhookClient() *http.Client {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 4 * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil {
			return nil, e
		}
		ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if e != nil || len(ips) == 0 {
			return nil, errors.New("Webhook DNS unavailable")
		}
		f, _ := a.failover()
		localIPs := localWebhookIPs()
		self := []string{f.WiredIP, f.CellularIP, "192.0.2.10"}
		if u, e := url.Parse(a.publicOrigin); e == nil {
			self = append(self, u.Hostname())
		}
		for _, ip := range ips {
			if !webhookIPAllowed(ip) || localIPs[ip.Unmap()] {
				return nil, errors.New("Webhook address rejected")
			}
			for _, own := range self {
				if parsed, e := netip.ParseAddr(own); e == nil && parsed.Unmap() == ip.Unmap() {
					return nil, errors.New("Webhook cannot target this host")
				}
			}
		}
		// Resolve once and dial the validated literal, preventing a second DNS lookup/rebinding.
		var last error
		for _, ip := range ips {
			conn, e := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if e == nil {
				return conn, nil
			}
			last = e
		}
		return nil, last
	}}
	return &http.Client{Transport: transport, Timeout: 6 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}
func (a *App) postForwardWebhook(ctx context.Context, j ForwardJob, r ForwardRule, m Message) (int, error) {
	if e := validateWebhookURL(r.URL); e != nil {
		return 0, e
	}
	payload := map[string]any{"event_id": j.ID, "event": "sms.received", "rule_id": r.ID, "message_id": m.ID, "sender": m.Sender, "text": m.Text, "timestamp": m.Timestamp, "received_at": m.FirstSeen, "forwarded_at": time.Now().UTC()}
	b, e := json.Marshal(payload)
	if e != nil {
		return 0, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", r.URL, bytes.NewReader(b))
	if e != nil {
		return 0, e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", j.ID)
	req.Header.Set("X-EC20-Event-ID", j.ID)
	checked, e := validateForwardRule(r)
	if e != nil {
		return 0, errors.New("Webhook Header 配置无效")
	}
	for _, h := range checked.Headers {
		req.Header.Set(h.Name, h.Value)
	}
	response, e := a.webhookClient.Do(req)
	if e != nil {
		return 0, errors.New("Webhook 请求未确认")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return response.StatusCode, nil
}
