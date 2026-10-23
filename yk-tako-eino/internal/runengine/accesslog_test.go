package runengine

import "testing"

// TestRedactAccessToken 钉访问日志的唯一职责：EventSource 只能把 token 放查询串，
// 写进日志就等于把凭证写进日志（doc/49 A12）。其余 query 必须原样留下——那是排障要看的。
func TestRedactAccessToken(t *testing.T) {
	cases := map[string]string{
		"/api/events?access_token=eyJhbGciOi.abc-123":       "/api/events?access_token=***",
		"/api/events?x=1&access_token=eyJhbGciOi.abc-123&y": "/api/events?x=1&access_token=***&y",
		"/api/events": "/api/events",
	}
	for in, want := range cases {
		if got := redactAccessToken(in); got != want {
			t.Fatalf("redactAccessToken(%q) = %q, want %q", in, got, want)
		}
	}
}
