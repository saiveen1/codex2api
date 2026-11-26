package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestResponsesWebSocketImageDeliveryViaHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	previousResin, previousWS := resinCfg.Load(), WebsocketExecuteFunc
	resinCfg.Store(nil)
	t.Cleanup(func() {
		resinCfg.Store(previousResin)
		WebsocketExecuteFunc = previousWS
	})

	var wsCalls, httpCalls atomic.Int32
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		wsCalls.Add(1)
		return nil, errors.New("unexpected upstream websocket attempt")
	}

	// A large deterministic base64 value catches truncation without external assets.
	result := strings.Repeat("QUJD", 32768)
	partial := "cGFydGlhbC1pbWFnZQ=="
	item := `{"id":"ig_delivery","type":"image_generation_call","status":"completed","result":"` + result + `","output_format":"png"}`
	partialEvent := `{"type":"response.image_generation_call.partial_image","item_id":"ig_delivery","output_index":0,"partial_image_index":0,"partial_image_b64":"` + partial + `"}`
	doneEvent := `{"type":"response.output_item.done","output_index":0,"item":` + item + `}`
	completedEvent := `{"type":"response.completed","response":{"id":"resp_image_delivery","status":"completed","output":[` + item + `],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	// An upstream frame after the terminal must never become a second delivery.
	sse := "data: " + partialEvent + "\n\ndata: " + doneEvent + "\n\ndata: " + completedEvent + "\n\ndata: " + doneEvent + "\n\n"

	account := &auth.Account{DBID: 930017, AccessToken: "synthetic-token", PlanType: "plus", AccountID: "synthetic-account"}
	key := clientPoolKey(account, "", codexTransportModeFromEnv())
	previousClient, hadClient := clientPool.Load(key)
	t.Cleanup(func() {
		if hadClient {
			clientPool.Store(key, previousClient)
		} else {
			clientPool.Delete(key)
		}
	})
	bodies := make(chan []byte, 4)
	entry := &poolEntry{client: &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		httpCalls.Add(1)
		if r.Method != http.MethodPost || r.URL.String() != CodexBaseURL+"/responses" {
			return nil, errors.New("rejected unexpected upstream HTTP destination or method")
		}
		body := readUpstreamRequestBody(r)
		select {
		case bodies <- body:
		default:
			return nil, errors.New("unexpected repeated upstream requests")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sse)), Request: r}, nil
	})}}
	entry.touch()
	clientPool.Store(key, entry)

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.5"})
	store.AddAccount(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	if proxyURL, usable := store.ResolveUsableProxyForAccount(account); !usable || proxyURL != "" {
		t.Fatalf("unexpected proxy route: usable=%v proxy=%q", usable, proxyURL)
	}
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if network != "tcp" || addr != server.Listener.Addr().String() {
			return nil, errors.New("rejected unexpected downstream network destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	conn, resp, err := dialer.Dial(wsURL, http.Header{"User-Agent": []string{"codex_cli_rs/0.110.0"}})
	if err != nil {
		t.Fatalf("dial local responses websocket: %v (response=%v)", err, resp)
	}
	defer conn.Close()
	request := `{"type":"response.create","model":"gpt-5.5","input":"draw a cat","tools":[{"type":"image_generation","partial_images":1}],"tool_choice":{"type":"image_generation"}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
		t.Fatalf("send image request: %v", err)
	}

	select {
	case body := <-bodies:
		if gjson.GetBytes(body, "type").Exists() || !gjson.GetBytes(body, "stream").Bool() || !responsesBodyRequestsImageGeneration(body) || !responsesBodyHasImageGenerationTool(body) {
			t.Fatalf("unexpected HTTP image request: %s", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no HTTP upstream request")
	}

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{"response.image_generation_call.partial_image", "response.output_item.done", "response.completed"}
	for i, wantType := range wantTypes {
		messageType, event, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read event %d (%s): %v", i, wantType, err)
		}
		if messageType != websocket.TextMessage || !gjson.ValidBytes(event) || gjson.GetBytes(event, "type").String() != wantType {
			t.Fatalf("event %d: unexpected frame type=%d event=%s", i, messageType, event)
		}
		switch wantType {
		case "response.image_generation_call.partial_image":
			if gjson.GetBytes(event, "item_id").String() != "ig_delivery" || !gjson.GetBytes(event, "partial_image_index").Exists() || gjson.GetBytes(event, "partial_image_index").Int() != 0 || gjson.GetBytes(event, "partial_image_b64").String() != partial {
				t.Fatalf("partial image changed: %s", event)
			}
		case "response.output_item.done":
			if gjson.GetBytes(event, "item.id").String() != "ig_delivery" || gjson.GetBytes(event, "item.type").String() != "image_generation_call" || gjson.GetBytes(event, "item.result").String() != result {
				t.Fatal("done image item was changed or truncated")
			}
		case "response.completed":
			if gjson.GetBytes(event, "response.id").String() != "resp_image_delivery" || gjson.GetBytes(event, "response.status").String() != "completed" || gjson.GetBytes(event, "response.output.#").Int() != 1 || gjson.GetBytes(event, "response.output.0.id").String() != "ig_delivery" || gjson.GetBytes(event, "response.output.0.type").String() != "image_generation_call" || gjson.GetBytes(event, "response.output.0.result").String() != result {
				t.Fatal("completed terminal or final image item was changed or truncated")
			}
		}
	}

	// done and completed contain the same logical item; only another frame is a duplicate.
	if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	_, extra, err := conn.ReadMessage()
	if err == nil {
		t.Fatalf("unexpected frame after completed terminal: %s", extra)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("unexpected connection end after completed terminal: %v", err)
	}
	if got := wsCalls.Load(); got != 0 {
		t.Errorf("upstream websocket attempts = %d, want 0", got)
	}
	if got := httpCalls.Load(); got != 1 {
		t.Errorf("upstream HTTP attempts = %d, want 1 (no replay)", got)
	}
}
