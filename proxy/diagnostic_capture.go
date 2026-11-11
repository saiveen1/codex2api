package proxy

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

// Capture before dispatch, including successful handshakes that may fail later in a stream.
// GetBody is a separate local reader: the live body is never consumed or replayed to a server.
func noteDiagnosticHTTPRequest(req *http.Request) {
	if req == nil || upstreamTraceFromContext(req.Context()) == nil {
		return
	}
	encoding := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding")))
	if encoding == "" {
		encoding = "identity"
	}
	capture := "decoded_transport"
	shape := &diagnosticRequestShape{Bytes: int(req.ContentLength), Inspection: "unavailable"}
	if encoding != "identity" && encoding != "zstd" && encoding != "gzip" {
		encoding, capture, shape.Inspection = "other", "unsupported_encoding", "unsupported_encoding"
	} else if req.GetBody == nil {
		capture = "body_unavailable"
	} else if req.ContentLength > diagnosticBodyLimit {
		capture, shape.Inspection = "size_limit", "size_limit"
	} else {
		reader, err := req.GetBody()
		if err != nil || reader == nil {
			capture, shape.Inspection = "read_error", "read_error"
		} else {
			defer reader.Close()
			var decoded io.Reader = io.LimitReader(reader, diagnosticBodyLimit+1)
			switch encoding {
			case "gzip":
				var decoder *gzip.Reader
				decoder, err = gzip.NewReader(decoded)
				if err == nil {
					defer decoder.Close()
					decoded = decoder
				}
			case "zstd":
				var decoder *zstd.Decoder
				decoder, err = zstd.NewReader(decoded, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(diagnosticBodyLimit), zstd.WithDecoderMaxWindow(diagnosticBodyLimit))
				if err == nil {
					defer decoder.Close()
					decoded = decoder
				}
			}
			if err != nil {
				capture, shape.Inspection = "decode_error", "decode_error"
			} else {
				var body []byte
				body, err = io.ReadAll(io.LimitReader(decoded, diagnosticBodyLimit+1))
				if len(body) > diagnosticBodyLimit {
					shape = &diagnosticRequestShape{Bytes: len(body), Inspection: "size_limit"}
					capture = "size_limit"
				} else if err != nil {
					shape = &diagnosticRequestShape{Bytes: len(body), Inspection: "read_error"}
					capture = "read_error"
					if encoding != "identity" {
						capture, shape.Inspection = "decode_error", "decode_error"
					}
				} else if contentType := strings.ToLower(req.Header.Get("Content-Type")); strings.HasPrefix(contentType, "multipart/") {
					shape = &diagnosticRequestShape{Bytes: len(body), Inspection: "non_json_body", Fingerprint: diagnosticFingerprint(string(body))}
				} else {
					shape = summarizeDiagnosticRequest(body)
				}
			}
		}
	}
	setDiagnosticRequest(req.Context(), req.ContentLength, encoding, capture, shape)
}

func setDiagnosticRequest(ctx context.Context, wireBytes int64, encoding, capture string, shape *diagnosticRequestShape) {
	a := upstreamTraceFromContext(ctx)
	if a == nil {
		return
	}
	a.mu.Lock()
	if a.current != nil {
		a.current.diagnostic.RequestBytes = wireBytes
		a.current.diagnostic.RequestEncoding = encoding
		a.current.diagnostic.RequestCapture = capture
		a.current.diagnostic.Request = shape
	}
	a.mu.Unlock()
}

// RecordDiagnosticWebsocketRequest observes the final response.create frame at its send site.
// Retain only its bounded shape, never the supplied byte slice or frame content.
func RecordDiagnosticWebsocketRequest(ctx context.Context, body []byte) {
	if upstreamTraceFromContext(ctx) == nil {
		return
	}
	setDiagnosticRequest(ctx, int64(len(body)), "websocket", "websocket_frame", summarizeDiagnosticRequest(body))
}

func noteDiagnosticStreamEvent(ctx context.Context, event string, body []byte) {
	if upstreamTraceFromContext(ctx) == nil {
		return
	}
	kind := normalizedUpstreamSSEEventType(event, body)
	errorValue := gjson.GetBytes(body, "error")
	responseError := gjson.GetBytes(body, "response.error")
	hasError := (errorValue.Exists() && errorValue.Type != gjson.Null) || (responseError.Exists() && responseError.Type != gjson.Null)
	if kind == "error" || kind == "response.failed" || kind == "response.incomplete" || hasError {
		noteDiagnosticErrorEvidence(ctx, body, "upstream_stream", nil)
	}
}

func noteDiagnosticErrorEvidence(ctx context.Context, body []byte, source string, readErr error) {
	a := upstreamTraceFromContext(ctx)
	if a == nil {
		return
	}
	detail := summarizeDiagnosticError(body)
	detail.Source = source
	if (source == "upstream_http" || source == "upstream_stream") && len(body) == 0 && readErr == nil {
		detail.Detail = "empty"
	}
	if readErr != nil {
		detail.ReadError = diagnosticTransportKind(readErr)
	}
	a.mu.Lock()
	if attempt := diagnosticAttemptForContext(ctx, a); attempt != nil {
		previous := attempt.diagnostic.Error
		// A later generic terminal must not erase the first actual stream error.
		if previous.Source != "upstream_stream" || previous.Detail == "unavailable" || source != "upstream_stream" {
			attempt.diagnostic.Error = detail
		}
	}
	a.mu.Unlock()
}

func noteDiagnosticStreamReadError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	a := upstreamTraceFromContext(ctx)
	if a == nil {
		return
	}
	a.mu.Lock()
	if attempt := diagnosticAttemptForContext(ctx, a); attempt != nil {
		attempt.diagnostic.Transport = diagnosticTransportKind(err)
		attempt.diagnostic.Error.ReadError = diagnosticTransportKind(err)
		if attempt.diagnostic.Error.Source == "" {
			attempt.diagnostic.Error.Source = "upstream_stream"
		}
	}
	a.mu.Unlock()
}

func noteDiagnosticResponseRead(ctx context.Context, body []byte, err error) {
	a := upstreamTraceFromContext(ctx)
	if a == nil {
		return
	}
	a.mu.Lock()
	status := 0
	if attempt := diagnosticAttemptForContext(ctx, a); attempt != nil {
		status = attempt.diagnostic.HTTPStatus
	}
	a.mu.Unlock()
	if status >= 400 {
		noteDiagnosticErrorEvidence(ctx, body, "upstream_http", err)
	} else {
		noteDiagnosticStreamEvent(ctx, "", body)
		noteDiagnosticStreamReadError(ctx, err)
	}
}

type diagnosticBoundAttemptKey struct{}

func bindDiagnosticAttempt(ctx context.Context) context.Context {
	a := upstreamTraceFromContext(ctx)
	if a == nil {
		return ctx
	}
	a.mu.Lock()
	attempt := a.current
	a.mu.Unlock()
	return context.WithValue(ctx, diagnosticBoundAttemptKey{}, attempt)
}

// Caller holds the trace mutex; an old stream keeps its original attempt even after reentry.
func diagnosticAttemptForContext(ctx context.Context, a *upstreamTraceAudit) *upstreamTraceAttempt {
	if attempt, ok := ctx.Value(diagnosticBoundAttemptKey{}).(*upstreamTraceAttempt); ok {
		return attempt
	}
	return a.current
}
