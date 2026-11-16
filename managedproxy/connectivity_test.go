package managedproxy

import "strings"

func connectivityErrorClass(err error) string {
	message := strings.ToLower(err.Error())
	for _, entry := range []struct{ needle, label string }{
		{"proxy authentication", "proxy_auth"}, {"forbidden", "proxy_forbidden"}, {"connection refused", "listener_refused"},
		{"no such host", "dns"}, {"timeout", "timeout"}, {"tls", "tls"}, {"eof", "proxy_eof"},
	} {
		if strings.Contains(message, entry.needle) {
			return entry.label
		}
	}
	return "transport"
}
