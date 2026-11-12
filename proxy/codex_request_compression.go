package proxy

import (
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Codex HTTP /responses request compression is optional transport encoding.
// Some valid multi-block frames are rejected by the receive path with a generic
// 400. Select identity BEFORE dispatch for those requests; never add a replay.
// This is a conservative gateway compatibility policy, not an upstream body limit.
// WebSocket frames and /responses/compact do not use this encoder.

// A zstd block regenerates at most 128 KiB. Skip larger inputs before allocating
// compression buffers; also verify the actual encoded frame below.
const codexRequestSingleBlockLimit = 128 << 10

// codexRequestCompressionEnabled reports whether compression is permitted.
//
// 环境变量 CODEX_REQUEST_COMPRESSION 显式设置时优先于系统设置：它是部署级逃生阀，
// 在 DB 不可达、后台打不开、或需要整机强制回退时仍然有效。未设置（或取值无法识别）
// 时以系统设置为准，由管理后台控制。
func codexRequestCompressionEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_REQUEST_COMPRESSION"))) {
	case "zstd", "on", "true", "1":
		return true
	case "off", "none", "false", "0", "plain":
		return false
	}
	return CurrentRuntimeSettings().CodexRequestCompression
}

var (
	codexZstdEncoderOnce sync.Once
	codexZstdEncoder     *zstd.Encoder
)

// codexRequestZstdEncoder 返回进程级共享编码器。zstd.Encoder 的 EncodeAll 是并发
// 安全的，因此不需要每请求新建（新建会分配整套窗口缓冲，在高 RPM 下代价显著）。
// SpeedDefault selects the library's default compression level.
func codexRequestZstdEncoder() *zstd.Encoder {
	codexZstdEncoderOnce.Do(func() {
		encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			// 参数是常量，构造失败不可达；留 nil 让调用方降级到明文而不是 panic。
			return
		}
		codexZstdEncoder = encoder
	})
	return codexZstdEncoder
}

// CompressCodexRequestBody chooses the first-send encoding for native Codex HTTP
// /responses. Empty encoding means the original bytes are sent without a
// Content-Encoding header. The switch does not bypass compatibility or size-
// benefit checks. WebSocket and /responses/compact do not call this function.
func CompressCodexRequestBody(body []byte) ([]byte, string) {
	if !shouldCompressCodexRequestBody(body) {
		return body, ""
	}
	encoder := codexRequestZstdEncoder()
	if encoder == nil {
		return body, ""
	}
	return selectCodexRequestEncoding(body, encoder.EncodeAll(body, nil))
}

func shouldCompressCodexRequestBody(body []byte) bool {
	return len(body) > 0 && len(body) <= codexRequestSingleBlockLimit && codexRequestCompressionEnabled()
}

// Only accept a smaller, complete single-block frame produced by our encoder.
// Keep this check even with the input bound: encoder options/versions can change
// block layout. No hand-edited frame or inferred decoded block offset is needed.
func selectCodexRequestEncoding(body, compressed []byte) ([]byte, string) {
	if len(compressed) == 0 || len(compressed) >= len(body) || len(body) > codexRequestSingleBlockLimit {
		return body, ""
	}
	var header zstd.Header
	if err := header.Decode(compressed); err != nil || header.Skippable || !header.FirstBlock.OK || !header.FirstBlock.Last {
		return body, ""
	}
	if header.HasFCS && header.FrameContentSize != uint64(len(body)) {
		return body, ""
	}
	frameSize := header.HeaderSize + 3 + header.FirstBlock.CompressedSize
	if header.HasCheckSum {
		frameSize += 4
	}
	if frameSize != len(compressed) {
		return body, ""
	}
	return compressed, "zstd"
}

// Account custom headers cannot override the finalized body's encoding/length.
func applyCodexBodyEncoding(headers http.Header, encoding string) {
	deleteHeaderCaseInsensitive(headers, "Content-Encoding")
	deleteHeaderCaseInsensitive(headers, "Content-Length")
	deleteHeaderCaseInsensitive(headers, "Transfer-Encoding")
	if encoding != "" {
		headers.Set("Content-Encoding", encoding)
	}
}
