package proxy

import "fmt"

// Do not forward raw worker/provider errors: they may contain credentials.
type webCheckTransportDiagnostic struct {
	ExceptionKind string `json:"exception_kind"`
	CurlCode      int    `json:"curl_code"`
	OSErrno       int    `json:"os_errno"`
	TransportHint string `json:"transport_hint"`
}

func (d webCheckTransportDiagnostic) safeCause() string {
	switch d.ExceptionKind {
	case "CurlError":
		if d.CurlCode <= 0 || d.CurlCode > 999 {
			return "curl 传输异常（无有效错误码）"
		}
		reason := map[int]string{5: "代理主机名解析失败", 6: "目标主机名解析失败", 7: "连接未能建立", 28: "操作超时", 35: "TLS 握手失败", 52: "收到空响应", 55: "发送数据失败", 56: "接收数据失败", 60: "对端证书验证失败", 97: "代理握手失败"}[d.CurlCode]
		if reason == "" {
			reason = "传输异常"
		}
		if hint := map[string]string{"peer_reset": "对端重置连接", "connection_refused": "连接被拒绝", "cert_expired": "证书过期", "issuer_unknown": "找不到证书签发者", "proxy_closed": "代理连接已关闭", "no_route": "无可用网络路由"}[d.TransportHint]; hint != "" {
			reason += "：" + hint
		}
		return fmt.Sprintf("curl(%d) %s", d.CurlCode, reason)
	case "TimeoutError", "ConnectionResetError", "ConnectionRefusedError", "OSError", "ValueError", "TypeError", "RuntimeError", "KeyError", "UnicodeDecodeError", "AttributeError", "gaierror", "OtherException":
		if d.OSErrno != 0 && d.OSErrno >= -4095 && d.OSErrno <= 65535 {
			return fmt.Sprintf("%s (errno=%d)", d.ExceptionKind, d.OSErrno)
		}
		return d.ExceptionKind
	}
	return ""
}
