package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/klauspost/compress/zstd"
)

func TestCodexFirstSendCompressionSelection(t *testing.T) {
	t.Setenv("CODEX_REQUEST_COMPRESSION", "on")
	for _, size := range []int{0, 1, 64, 4096, 65535, 65536, 131071, 131072, 131073, 140161, 300161, 2 << 20} {
		for _, noise := range []bool{false, true} {
			body := bytes.Repeat([]byte("a"), size)
			if noise {
				_, _ = rand.New(rand.NewSource(7)).Read(body)
			}
			for name, compress := range map[string]func([]byte) ([]byte, string){"normal": CompressCodexRequestBody, "pipeline": compressPipelineRequestBody} {
				t.Run(fmt.Sprintf("%s/%d/noise=%v", name, size, noise), func(t *testing.T) {
					got, encoding := compress(body)
					if size > codexRequestSingleBlockLimit || size <= 1 || noise {
						if encoding != "" || !bytes.Equal(got, body) {
							t.Fatal("expected exact identity body")
						}
						if size > 0 && &got[0] != &body[0] {
							t.Fatal("identity path copied body")
						}
						return
					}
					if encoding != "zstd" || len(got) >= len(body) {
						t.Fatal("small compressible body did not save bytes")
					}
					var h zstd.Header
					if err := h.Decode(got); err != nil || !h.FirstBlock.Last {
						t.Fatal("not one block", err)
					}
					if !bytes.Equal(decodeZstd(t, got), body) {
						t.Fatal("decoded bytes changed")
					}
				})
			}
		}
	}
	for _, mode := range []string{"off", "false", "0", "plain"} {
		t.Setenv("CODEX_REQUEST_COMPRESSION", mode)
		body := bytes.Repeat([]byte("x"), 4096)
		for _, compress := range []func([]byte) ([]byte, string){CompressCodexRequestBody, compressPipelineRequestBody} {
			got, encoding := compress(body)
			if encoding != "" || !bytes.Equal(got, body) {
				t.Fatal("off switch changed body")
			}
		}
	}
}

func TestCodexFirstSendRejectsNonSingleFrames(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 100000)
	single := codexRequestZstdEncoder().EncodeAll(body, nil)
	var stream bytes.Buffer
	enc, err := zstd.NewWriter(&stream, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<16))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = enc.Write(body[:50000]); err != nil {
		t.Fatal(err)
	}
	if err = enc.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err = enc.Write(body[50000:]); err != nil {
		t.Fatal(err)
	}
	if err = enc.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decodeZstd(t, stream.Bytes()), body) {
		t.Fatal("invalid multiblock fixture")
	}
	for name, wire := range map[string][]byte{
		"multiblock-below-limit": stream.Bytes(),
		"concatenated":           append(append([]byte(nil), single...), single...),
		"truncated":              single[:len(single)-1],
		"invalid":                []byte("not zstd"),
		"empty":                  nil,
		"expanded":               bytes.Repeat([]byte("z"), len(body)),
	} {
		t.Run(name, func(t *testing.T) {
			got, encoding := selectCodexRequestEncoding(body, wire)
			if encoding != "" || !bytes.Equal(got, body) {
				t.Fatal("unqualified frame selected")
			}
		})
	}
	if _, encoding := selectCodexRequestEncoding(body[:99999], single); encoding != "" {
		t.Fatal("wrong FCS accepted")
	}
}

func TestCodexFirstSendExecutorNoRecoveryReplay(t *testing.T) {
	t.Setenv("CODEX_REQUEST_COMPRESSION", "on")
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	old, oldResin := CurrentRuntimeSettings(), resinCfg.Load()
	next := old
	next.CodexForceWebsocket = false
	next.CodexTelemetryEnabled = false
	ApplyRuntimeSettings(next)
	resinCfg.Store(nil)
	t.Cleanup(func() { ApplyRuntimeSettings(old); resinCfg.Store(oldResin) })
	for _, pipeline := range []bool{false, true} {
		for _, large := range []bool{false, true} {
			t.Run(fmt.Sprintf("pipeline=%v/large=%v", pipeline, large), func(t *testing.T) {
				a := &auth.Account{DBID: 930001, AccessToken: "test-token", CustomHeaders: map[string]string{"Content-Encoding": "gzip", "Content-Length": "1", "Transfer-Encoding": "chunked"}}
				ctx := context.Background()
				var p *ImagePipeline
				if pipeline {
					p = &ImagePipeline{slots: make(chan struct{}, 1), dir: t.TempDir()}
					defer p.Close()
					ctx = context.WithValue(ctx, imagePipelineKey{}, p)
					if err := p.Acquire(ctx); err != nil {
						t.Fatal(err)
					}
				}
				size := 1024
				if large {
					size = 140000
				}
				body := []byte(`{"model":"gpt-5.5","input":"` + strings.Repeat("x", size) + `","previous_response_id":"remove"}`)
				key := clientPoolKey(a, "", codexTransportModeFromEnv())
				prior, exists := clientPool.Load(key)
				calls := 0
				clientPool.Store(key, &poolEntry{client: &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
					calls++
					wire, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					_ = r.Body.Close()
					if r.ContentLength != int64(len(wire)) || r.GetBody == nil {
						t.Fatal("wrong length/GetBody")
					}
					copyBody, err := r.GetBody()
					if err != nil {
						t.Fatal(err)
					}
					copyWire, err := io.ReadAll(copyBody)
					_ = copyBody.Close()
					if err != nil || !bytes.Equal(wire, copyWire) {
						t.Fatal("GetBody differs")
					}
					if r.Header.Get("Content-Length") != "" || r.Header.Get("Transfer-Encoding") != "" {
						t.Fatal("custom transport header survived")
					}
					if large {
						if r.Header.Get("Content-Encoding") != "" {
							t.Fatal("large first send was compressed")
						}
					} else {
						if r.Header.Get("Content-Encoding") != "zstd" {
							t.Fatal("small first send was not compressed")
						}
						wire = decodeZstd(t, wire)
					}
					var obj map[string]any
					if err = json.Unmarshal(wire, &obj); err != nil {
						t.Fatal(err)
					}
					if obj["input"] != strings.Repeat("x", size) || obj["previous_response_id"] != nil {
						t.Fatal("final JSON changed")
					}
					if p != nil && len(p.slots) != 0 {
						t.Fatal("pipeline memory slot retained")
					}
					return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"detail":"Bad Request"}`)), Request: r}, nil
				})}})
				t.Cleanup(func() {
					if exists {
						clientPool.Store(key, prior)
					} else {
						clientPool.Delete(key)
					}
				})
				resp, err := ExecuteRequest(ctx, a, body, "", "", "", nil, nil, false)
				if err != nil {
					t.Fatal(err)
				}
				got, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if err != nil || resp.StatusCode != 400 || string(got) != `{"detail":"Bad Request"}` || calls != 1 {
					t.Fatal("400 was replayed/hidden", calls, err)
				}
				if p != nil {
					files, err := os.ReadDir(p.dir)
					if err != nil || len(files) != 0 {
						t.Fatal("pipeline files leaked")
					}
				}
			})
		}
	}
}

func TestCodexFirstSendBodyHeaders(t *testing.T) {
	for _, encoding := range []string{"", "zstd"} {
		h := http.Header{"content-encoding": {"gzip"}, "Content-Encoding": {"br"}, "content-length": {"999"}, "Transfer-Encoding": {"chunked"}, "X-Test": {"keep"}}
		applyCodexBodyEncoding(h, encoding)
		if h.Get("Content-Encoding") != encoding || h.Get("X-Test") != "keep" {
			t.Fatal(h)
		}
		for k := range h {
			if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") || k == "content-encoding" {
				t.Fatal("stale header", k)
			}
		}
	}
}

func BenchmarkCodexFirstSendLargeBody(b *testing.B) {
	b.Setenv("CODEX_REQUEST_COMPRESSION", "on")
	body := bytes.Repeat([]byte("benchmark-data "), 150000)
	enc := codexRequestZstdEncoder()
	b.Run("previous_encode_all", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = enc.EncodeAll(body, nil)
		}
	})
	b.Run("first_send_selection", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, _ = CompressCodexRequestBody(body)
		}
	})
}

func TestCodexFirstSendExplicitRLEFrame(t *testing.T) {
	// Single segment, FCS=512, no checksum; one RLE block storing 512 'a' bytes.
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x60, 0x00, 0x01, 0x03, 0x10, 0x00, 'a'}
	body := bytes.Repeat([]byte("a"), 512)
	if !bytes.Equal(decodeZstd(t, frame), body) {
		t.Fatal("invalid RLE fixture")
	}
	var h zstd.Header
	if err := h.Decode(frame); err != nil || !h.FirstBlock.Compressed || (frame[h.HeaderSize]>>1)&3 != 1 || h.FirstBlock.CompressedSize != 1 || h.FirstBlock.DecompressedSize != 512 {
		t.Fatalf("RLE header: %+v, %v", h, err)
	}
	got, encoding := selectCodexRequestEncoding(body, frame)
	if encoding != "zstd" || !bytes.Equal(got, frame) {
		t.Fatal("single RLE frame length misclassified")
	}
}

func TestCodexFirstSendCompactBodyHeaders(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	oldResin := resinCfg.Load()
	resinCfg.Store(nil)
	t.Cleanup(func() { resinCfg.Store(oldResin) })
	a := &auth.Account{DBID: 930002, AccessToken: "test-token", CustomHeaders: map[string]string{"Content-Encoding": "zstd", "Content-Length": "1", "Transfer-Encoding": "chunked"}}
	key := clientPoolKey(a, "", codexTransportModeFromEnv())
	previous, exists := clientPool.Load(key)
	t.Cleanup(func() {
		if exists {
			clientPool.Store(key, previous)
		} else {
			clientPool.Delete(key)
		}
	})
	calls := 0
	clientPool.Store(key, &poolEntry{client: &http.Client{Transport: diagnosticRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		wire, err := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if err != nil || !json.Valid(wire) || r.ContentLength != int64(len(wire)) || r.GetBody == nil {
			t.Fatal("invalid compact body", err)
		}
		for _, name := range []string{"Content-Encoding", "Content-Length", "Transfer-Encoding"} {
			if r.Header.Get(name) != "" {
				t.Fatal("conflicting compact header", name)
			}
		}
		if r.URL.Path != "/backend-api/codex/responses/compact" {
			t.Fatal(r.URL.Path)
		}
		copyBody, err := r.GetBody()
		if err != nil {
			t.Fatal(err)
		}
		copyWire, err := io.ReadAll(copyBody)
		_ = copyBody.Close()
		if err != nil || !bytes.Equal(wire, copyWire) {
			t.Fatal("compact GetBody differs")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
	})}})
	resp, err := ExecuteCompactRequest(context.Background(), a, []byte(`{"model":"gpt-5.5","input":"hello"}`), "", "", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if calls != 1 {
		t.Fatal("unexpected compact calls", calls)
	}
}
