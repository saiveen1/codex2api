package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

type upstreamTraceContextKey struct{}
type upstreamTraceAttempt struct {
	round      upstreamRoundTiming
	accountID  int64
	requestID  string
	proxy      auth.ProxyAuditLabel
	diagnostic diagnosticAttempt
	// upstreamTurnState 是上游响应回带的 X-Codex-Turn-State，空串表示没有。
	upstreamTurnState string
}

type upstreamTraceSnapshot struct {
	round             upstreamRoundTiming
	RequestID         string
	accountID         int64
	UpstreamRequestID string
	Proxy             auth.ProxyAuditLabel
	InjectedTurnState string
	UpstreamTurnState string
	Diagnostic        diagnosticAttempt
	ParentRequestID   string
}

func snapshotUpstreamTrace(ctx context.Context) upstreamTraceSnapshot {
	a := upstreamTraceFromContext(ctx)
	if a == nil {
		return upstreamTraceSnapshot{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	result := upstreamTraceSnapshot{RequestID: a.requestID, ParentRequestID: a.parentRequestID}
	if a.current != nil {
		result.accountID = a.current.accountID
		result.round = a.current.round
		result.UpstreamRequestID = a.current.requestID
		result.Proxy = a.current.proxy
		result.UpstreamTurnState = a.current.upstreamTurnState
		result.Diagnostic = a.current.diagnostic
	}
	return result
}

func (s upstreamTraceSnapshot) apply(input *database.UsageLogInput) {
	input.RequestID = s.RequestID
	if s.accountID == input.AccountID {
		input.UpstreamRequestID = s.UpstreamRequestID
		input.UpstreamProxyID = s.Proxy.ID
		input.UpstreamProxyName = s.Proxy.Name
		input.InjectedTurnState = s.InjectedTurnState
		input.UpstreamTurnState = s.UpstreamTurnState
		input.TraceAttemptIndex = s.Diagnostic.Index
		input.TraceHTTPStatus = s.Diagnostic.HTTPStatus
		applyDiagnosticTraceSnapshot(s, input)
	}
}

type upstreamTraceAudit struct {
	mu                    sync.Mutex
	requestID             string
	parentRequestID       string
	store                 *auth.Store
	current               *upstreamTraceAttempt
	started               time.Time
	attempts              int
	diagnosticFailed      bool
	diagnosticOutcome     int
	diagnosticFinal       string
	diagnosticDB          *database.DB
	diagnosticUsageLogged bool
	dispatchDecision      *dispatchDecision
	dispatchStream        bool
}

func upstreamTraceFromContext(ctx context.Context) *upstreamTraceAudit {
	if ctx == nil {
		return nil
	}
	a, _ := ctx.Value(upstreamTraceContextKey{}).(*upstreamTraceAudit)
	return a
}

func attachUpstreamTrace(c *gin.Context, store *auth.Store) {
	if c == nil || c.Request == nil {
		return
	}
	if upstreamTraceFromContext(c.Request.Context()) != nil {
		return
	}
	a := &upstreamTraceAudit{requestID: NewUpstreamSessionUUID(), store: store, started: time.Now()}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), upstreamTraceContextKey{}, a))
	c.Header("X-Codex2API-Request-ID", a.requestID)
}

func resetUpstreamRequestTrace(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	if a := upstreamTraceFromContext(c.Request.Context()); a != nil {
		// A multiplexed WS lane must not mutate another lane's shared context.
		next := &upstreamTraceAudit{requestID: NewUpstreamSessionUUID(), store: a.store, started: time.Now()}
		next.diagnosticDB = a.diagnosticDB
		next.parentRequestID = a.parentRequestID
		if next.parentRequestID == "" {
			next.parentRequestID = a.requestID
		}
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), upstreamTraceContextKey{}, next))
	}
}

func resetUpstreamAttemptTrace(ctx context.Context) {
	if a := upstreamTraceFromContext(ctx); a != nil {
		a.mu.Lock()
		a.current = nil
		a.mu.Unlock()
	}
}

func beginUpstreamTrace(ctx context.Context, account *auth.Account, proxyURL string, ws bool) func(*http.Response) {
	recordUpstreamAttempt(ctx)
	a := upstreamTraceFromContext(ctx)
	if a == nil || account == nil {
		return func(*http.Response) {}
	}
	boundProxy, accountBound := AccountBoundEgress(ctx)
	if accountBound {
		proxyURL = boundProxy
	}
	label := a.store.ProxyAuditForURL(proxyURL)
	if !accountBound && ws && proxyURL == "" {
		label = auth.ProxyAuditLabel{Name: "unknown"}
	}
	if !accountBound && resinCarriesEgress(account) {
		label = auth.ProxyAuditLabel{Name: "resin"}
	}
	label.Name = security.MaskSensitiveData(label.Name)
	attempt := &upstreamTraceAttempt{accountID: account.ID(), proxy: label}
	if ws {
		attempt.round.started = time.Now()
	}
	a.mu.Lock()
	a.attempts++
	attempt.diagnostic = diagnosticAttempt{Index: a.attempts, RequestBytes: -1, Error: diagnosticError{Detail: "unavailable"}}
	attempt.diagnostic.Protocol = "http"
	if ws {
		attempt.diagnostic.Protocol = "websocket"
	}
	a.current = attempt
	a.mu.Unlock()
	header := account.GetUpstreamRequestIDHeader()
	return func(resp *http.Response) {
		if resp != nil {
			a.mu.Lock()
			attempt.diagnostic.HTTPStatus = resp.StatusCode
			a.mu.Unlock()
		}
		if resp == nil || ws {
			return
		} // A WS handshake ID is not a per-turn ID; WS turn state arrives per frame, see ObserveCodexTurnStateFrame.
		turnState := observedCodexTurnState(resp.Header.Get(codexTurnStateHeader))
		id := ""
		if header != "" && auth.ValidateUpstreamRequestIDHeader(header) == nil {
			id = resp.Header.Get(header)
		}
		if strings.TrimSpace(id) == "" {
			for _, name := range []string{"X-Request-Id", "Request-Id", "X-Openai-Request-Id", "X-Oai-Request-Id", "X-Goog-Request-Id"} {
				if id = resp.Header.Get(name); strings.TrimSpace(id) != "" {
					break
				}
			}
		}
		id = security.SafeTruncate(strings.TrimSpace(id), 128)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.current == attempt {
			attempt.requestID = id
			if turnState != "" {
				attempt.upstreamTurnState = turnState
			}
		}
	}
}

// noteUpstreamTurnState 把上游回带的 turn state 记到当前尝试上；WS 路径逐帧调用，
// 后到的值覆盖先到的。
func noteUpstreamTurnState(ctx context.Context, state string) {
	a := upstreamTraceFromContext(ctx)
	if a == nil || state == "" {
		return
	}
	a.mu.Lock()
	if a.current != nil {
		a.current.upstreamTurnState = state
	}
	a.mu.Unlock()
}

func doTracedUpstreamRequest(client *http.Client, req *http.Request, account *auth.Account, proxyURL string) (*http.Response, error) {
	record := beginUpstreamTrace(req.Context(), account, proxyURL, false)
	noteDiagnosticHTTPRequest(req)
	if account != nil && (!account.IsRelayStyle() || account.IsOpenAIResponsesAPI()) {
		startUpstreamRound(req.Context())
	}
	resp, err := client.Do(req)
	record(resp)
	noteDiagnosticHTTPDispatch(req, resp, err)
	return resp, err
}

func populateUpstreamTrace(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	if input.RequestID != "" {
		return
	} // Hidden continuation rounds carry their own snapshot.
	a := upstreamTraceFromContext(c.Request.Context())
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	input.RequestID = a.requestID
	if current := a.current; current != nil && current.accountID == input.AccountID {
		input.UpstreamRequestID = current.requestID
		input.UpstreamProxyID = current.proxy.ID
		input.UpstreamProxyName = current.proxy.Name
		input.UpstreamTurnState = current.upstreamTurnState
		input.TraceAttemptIndex = current.diagnostic.Index
		input.TraceHTTPStatus = current.diagnostic.HTTPStatus
		// Folded rows already carry the snapshot matching their terminal usage.
		if input.UpstreamRoundIndex == 0 {
			current.round.apply(input)
		}
	}
}
