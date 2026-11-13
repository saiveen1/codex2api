package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestURLs(t *testing.T) {
	for _, tc := range []struct {
		url       string
		allow, ok bool
	}{
		{"https://example.com/prefix/", false, true}, {"http://127.0.0.1:123/p", false, true}, {"http://localhost", false, true}, {"http://[::1]", false, true}, {"http://example.com", false, false}, {"http://example.com", true, true}, {"https://user:pass@example.com", false, false}, {"https://example.com/?x=1", false, false}, {"https://example.com/#", false, false}, {"ftp://localhost", true, false},
	} {
		got, err := ValidateBaseURL(tc.url, tc.allow)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %q %v", tc.url, got, err)
		}
		if err == nil && strings.HasSuffix(got, "/") {
			t.Errorf("trailing slash: %s", got)
		}
	}
}
func TestOAuthPathsAndResults(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic" {
			t.Error("wrong authorization")
		}
		switch r.URL.Path {
		case "/prefix/api/v1/oauth/preview":
			fmt.Fprint(w, `{"ok":true,"data":{"valid":false,"count":0,"rows":[],"errors":[{"line":1,"code":"invalid_input"}]}}`)
		case "/prefix/api/v1/oauth/proxy-check":
			var payload map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&payload)
			var opts map[string]any
			_ = json.Unmarshal(payload["options"], &opts)
			if len(payload) != 1 || opts["workspace_policy"] != nil || opts["proxy"] == nil {
				t.Error("invalid proxy check payload")
			}
			fmt.Fprint(w, `{"ok":true,"data":{"usable":false,"requested_country":"US","openai_reachable":false,"checked_at":1,"elapsed_ms":2,"error":{"code":"proxy_country_mismatch","message":"secret","source":"execution","action":"fix_request","retryable":false,"retry_after":null}}}`)
		case "/prefix/api/v1/oauth/requests/r1", "/prefix/api/v1/oauth/batches/b1", "/prefix/api/v1/oauth/batches/b1/cancel", "/prefix/api/v1/oauth/batches":
			if r.Method == "GET" {
				b, _ := io.ReadAll(r.Body)
				if len(b) != 0 {
					t.Error("GET has body")
				}
			} else if strings.HasSuffix(r.URL.Path, "/cancel") {
				b, _ := io.ReadAll(r.Body)
				if string(b) != "{}" {
					t.Error("cancel body")
				}
			}
			fmt.Fprint(w, `{"ok":true,"data":{"batch_id":"b1","request_id":"r1","state":"queued","total":1,"succeeded":0,"failed":0,"pending":1,"items":[]}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer s.Close()
	c, err := NewOAuth(s.URL+"/prefix/", "synthetic", false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	p, err := c.Preview(ctx, SubmitRequest{})
	if err != nil || p.Valid || p.Errors[0].Code != "invalid_input" {
		t.Fatalf("preview %#v %v", p, err)
	}
	pc, err := c.CheckProxy(ctx, Options{Country: "US", Proxy: json.RawMessage(`"http://proxy"`), WorkspacePolicy: "team_preferred"})
	if err != nil || pc.Usable || pc.Error == nil || strings.Contains(pc.Error.Message, "secret") {
		t.Fatalf("proxy %#v %v", pc, err)
	}
	if _, err = c.Submit(ctx, SubmitRequest{RequestID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Batch(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Lookup(ctx, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Cancel(ctx, "b1"); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Batch(ctx, "../bad"); err == nil {
		t.Fatal("invalid id accepted")
	}
}
func TestOAuthUnknownAndRedirect(t *testing.T) {
	leaked := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = true }))
	defer target.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/oauth/batches":
			fmt.Fprint(w, `{"ok":true,"data":{}}`)
		default:
			http.Redirect(w, r, target.URL, 302)
		}
	}))
	defer s.Close()
	c, _ := NewOAuth(s.URL, "secret", false)
	_, err := c.Submit(context.Background(), SubmitRequest{RequestID: "r1"})
	e, ok := err.(*Error)
	if !ok || !e.Uncertain || e.Action != "query_original" {
		t.Fatalf("unexpected error %v", err)
	}
	_, err = c.Quota(context.Background())
	if err == nil || leaked {
		t.Fatal("redirect followed")
	}
}
func TestDelivery(t *testing.T) {
	batch := Batch{Items: []BatchItem{{ItemID: "a", Line: 1, State: "succeeded"}, {ItemID: "b", Line: 2, State: "succeeded"}, {ItemID: "c", Line: 3, State: "succeeded"}}}
	original := Result{Data: json.RawMessage(`{"type":"sub2api-data","version":1,"accounts":[{"token":"b"},{"token":"a"}]}`), Delivery: Delivery{IncludedCount: 2, ExpiredCount: 1, ExpiredItemIDs: []string{"c"}, Items: []DeliveryItem{{ItemID: "a", Line: 1, AccountIndex: 1}, {ItemID: "b", Line: 2, AccountIndex: 0}}}}
	a, err := ValidateDelivery(original, batch)
	if err != nil || !strings.Contains(string(a[1]), `"a"`) {
		t.Fatal("reordered mapping failed", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Result)
	}{
		{"duplicate", func(r *Result) { r.Delivery.Items[1].ItemID = "a" }}, {"duplicate-index", func(r *Result) { r.Delivery.Items[1].AccountIndex = 1 }}, {"out-of-range", func(r *Result) { r.Delivery.Items[0].AccountIndex = 2 }}, {"wrong-item", func(r *Result) { r.Delivery.Items[0].ItemID = "x" }}, {"wrong-line", func(r *Result) { r.Delivery.Items[0].Line = 9 }}, {"expired-overlap", func(r *Result) { r.Delivery.ExpiredItemIDs[0] = "a" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := original
			r.Delivery.Items = append([]DeliveryItem(nil), original.Delivery.Items...)
			r.Delivery.ExpiredItemIDs = append([]string(nil), original.Delivery.ExpiredItemIDs...)
			tc.mutate(&r)
			if _, err := ValidateDelivery(r, batch); err == nil {
				t.Fatal("accepted invalid mapping")
			}
		})
	}
}
func TestMalformedEnvelopesAndCancellation(t *testing.T) {
	for _, body := range []string{`{}`, `{"ok":true,"data":null}`, `{"ok":true,"data":{"batch_id":"b"}}`, `{"ok":false,"error":{}}`} {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		c, _ := NewOAuth(s.URL, "token", false)
		_, err := c.Submit(context.Background(), SubmitRequest{RequestID: "r"})
		s.Close()
		e, ok := err.(*Error)
		if !ok || !e.Uncertain || e.Action != "query_original" {
			t.Fatalf("malformed response treated as certain: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c, _ := NewOAuth("http://127.0.0.1:1", "token", false)
	_, err := c.Submit(ctx, SubmitRequest{RequestID: "r"})
	if e, ok := err.(*Error); !ok || !e.Uncertain {
		t.Fatal("cancelled submit must be uncertain")
	}
}
func TestErrorSanitization(t *testing.T) {
	for _, code := range []string{"rate_limited", "synthetic-token"} {
		raw, _ := json.Marshal(map[string]any{"code": code, "message": "synthetic-secret", "source": "synthetic-secret", "action": "retry_same_request", "retryable": true, "retry_after": 5})
		var e Error
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(e)
		if strings.Contains(string(out), "synthetic") {
			t.Fatal("upstream error leaked arbitrary text")
		}
		if code == "rate_limited" && (!e.Retryable || e.RetryAfter == nil) {
			t.Fatal("valid retry metadata lost")
		}
		if code != "rate_limited" && (e.Retryable || e.Action != "query_original") {
			t.Fatal("unknown error enabled retries")
		}
	}
}
