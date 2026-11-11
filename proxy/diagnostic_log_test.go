package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type diagnosticWarningCounter struct {
	failures   int
	recoveries int
}

func (c *diagnosticWarningCounter) WarnFailure()   { c.failures++ }
func (c *diagnosticWarningCounter) WarnRecovered() { c.recoveries++ }

func TestDiagnosticFileLoggerLineRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	logger := newTestDiagnosticLogger(path, 1024, 3)
	defer func() { _ = logger.close() }()

	line, err := marshalDiagnosticLine(map[string]any{"kind": "upstream", "status": 502})
	if err != nil {
		t.Fatalf("marshalDiagnosticLine() error = %v", err)
	}
	if err := logger.write(line); err != nil {
		t.Fatalf("write() error = %v", err)
	}
	if err := logger.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !bytes.HasSuffix(content, []byte{'\n'}) {
		t.Fatalf("log content is not newline terminated: %q", content)
	}
	var got map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(content), &got); err != nil {
		t.Fatalf("stored record is not JSON: %v", err)
	}
	if got["kind"] != "upstream" || got["status"] != float64(502) {
		t.Fatalf("stored record = %#v", got)
	}
}

func TestMarshalDiagnosticLineSizeLimit(t *testing.T) {
	fits, err := marshalDiagnosticLine(map[string]string{"payload": strings.Repeat("a", diagnosticMaxLineSize-32)})
	if err != nil {
		t.Fatalf("marshal line below limit: %v", err)
	}
	if len(fits) > diagnosticMaxLineSize {
		t.Fatalf("line size = %d, limit = %d", len(fits), diagnosticMaxLineSize)
	}

	_, err = marshalDiagnosticLine(map[string]string{"payload": strings.Repeat("a", diagnosticMaxLineSize)})
	if !errors.Is(err, errDiagnosticLineTooLarge) {
		t.Fatalf("marshal oversized line error = %v, want %v", err, errDiagnosticLineTooLarge)
	}
}

func TestDiagnosticFileLoggerRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	logger := newTestDiagnosticLogger(path, 12, 3)
	defer func() { _ = logger.close() }()

	for _, line := range [][]byte{[]byte("one\n"), []byte("two\n"), []byte("three\n"), []byte("four\n"), []byte("five\n"), []byte("six\n"), []byte("seven\n")} {
		if err := logger.write(line); err != nil {
			t.Fatalf("write(%q) error = %v", line, err)
		}
	}
	if err := logger.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}

	for _, name := range []string{path, path + ".1", path + ".2", path + ".3"} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("Stat(%q) error = %v", name, err)
		}
		if info.Size() > 12 {
			t.Fatalf("%q size = %d, want <= 12", name, info.Size())
		}
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(current) error = %v", err)
	}
	newestBackup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("ReadFile(newest backup) error = %v", err)
	}
	oldestBackup, err := os.ReadFile(path + ".3")
	if err != nil {
		t.Fatalf("ReadFile(oldest backup) error = %v", err)
	}
	if string(current) != "seven\n" || string(newestBackup) != "five\nsix\n" || string(oldestBackup) != "one\ntwo\n" {
		t.Fatalf("current/newest/oldest = %q / %q / %q", current, newestBackup, oldestBackup)
	}
}

func TestDiagnosticFileLoggerConcurrentWritesAreJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	logger := newTestDiagnosticLogger(path, 1024*1024, 3)
	defer func() { _ = logger.close() }()

	const writers = 24
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			line, err := marshalDiagnosticLine(map[string]int{"index": index})
			if err != nil {
				t.Errorf("marshalDiagnosticLine() error = %v", err)
				return
			}
			if err := logger.write(line); err != nil {
				t.Errorf("write() error = %v", err)
			}
		}(i)
	}
	wg.Wait()
	if err := logger.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(lines) != writers {
		t.Fatalf("record count = %d, want %d", len(lines), writers)
	}
	seen := make(map[int]bool, writers)
	for _, line := range lines {
		var record struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid JSONL record %q: %v", line, err)
		}
		seen[record.Index] = true
	}
	if len(seen) != writers {
		t.Fatalf("distinct record count = %d, want %d", len(seen), writers)
	}
}

func TestWriteDiagnosticDisabledDoesNotCreateFile(t *testing.T) {
	original := diagnosticLogger
	defer func() {
		_ = diagnosticLogger.close()
		diagnosticLogger = original
	}()

	path := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	diagnosticLogger = newTestDiagnosticLogger(path, 1024, 3)
	t.Setenv("LOG_DISABLED", "true")
	writeDiagnostic(map[string]string{"secret": "must not be written"})

	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Stat(%q) error = %v, want not exist", path, err)
	}
}

func TestDiagnosticFileLoggerCloseAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	logger := newTestDiagnosticLogger(path, 1024, 3)
	defer func() { _ = logger.close() }()

	if err := logger.write([]byte("first\n")); err != nil {
		t.Fatalf("first write() error = %v", err)
	}
	if err := logger.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}
	if err := logger.write([]byte("second\n")); err != nil {
		t.Fatalf("reopen write() error = %v", err)
	}
	if err := logger.close(); err != nil {
		t.Fatalf("second close() error = %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got, want := string(content), "first\nsecond\n"; got != want {
		t.Fatalf("file content = %q, want %q", got, want)
	}
}

func TestDiagnosticFileLoggerRecoversAfterOpenFailure(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile(blocker) error = %v", err)
	}
	path := filepath.Join(blocked, "diagnostics.jsonl")
	warnings := &diagnosticWarningCounter{}
	logger := newTestDiagnosticLogger(path, 1024, 3)
	logger.sink = warnings
	defer func() { _ = logger.close() }()

	if err := logger.write([]byte("lost\n")); err == nil {
		t.Fatal("write() after blocked open error = nil")
	}
	if !logger.failureReported || warnings.failures != 1 {
		t.Fatalf("failure state/count = %t/%d, want true/1", logger.failureReported, warnings.failures)
	}
	if err := logger.write([]byte("lost-again\n")); err == nil {
		t.Fatal("second failed write() error = nil")
	}
	if warnings.failures != 1 {
		t.Fatalf("failure warnings = %d, want 1 per streak", warnings.failures)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatalf("Remove(blocker) error = %v", err)
	}
	if err := logger.write([]byte("recovered\n")); err != nil {
		t.Fatalf("write() after recovery error = %v", err)
	}
	if logger.failureReported || warnings.recoveries != 1 {
		t.Fatalf("recovery state/count = %t/%d, want false/1", logger.failureReported, warnings.recoveries)
	}
	if err := logger.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got, want := string(content), "recovered\n"; got != want {
		t.Fatalf("file content = %q, want %q", got, want)
	}
}

func TestDiagnosticFileLoggerPartialWriteRotatesBeforeRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostics.jsonl")
	if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	logger := newTestDiagnosticLogger(path, 1024, 3)
	logger.file = &shortDiagnosticFile{}

	if err := logger.write([]byte("record\n")); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write error = %v, want %v", err, io.ErrShortWrite)
	}
	if !logger.rotateBeforeWrite {
		t.Fatal("rotateBeforeWrite = false after short write")
	}
	if err := logger.write([]byte("retry\n")); err != nil {
		t.Fatalf("retry write() error = %v", err)
	}
	if err := logger.close(); err != nil {
		t.Fatalf("close() error = %v", err)
	}

	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("ReadFile(backup) error = %v", err)
	}
	if got, want := string(backup), "partial"; got != want {
		t.Fatalf("backup content = %q, want %q", got, want)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(current) error = %v", err)
	}
	if got, want := string(content), "retry\n"; got != want {
		t.Fatalf("current content = %q, want %q", got, want)
	}
}

func newTestDiagnosticLogger(path string, maxBytes int64, backups int) *diagnosticFileLogger {
	return &diagnosticFileLogger{path: path, maxBytes: maxBytes, backups: backups}
}

type shortDiagnosticFile struct{}

func (*shortDiagnosticFile) Write(data []byte) (int, error) { return len(data) - 1, nil }
func (*shortDiagnosticFile) Stat() (os.FileInfo, error)     { return zeroDiagnosticFileInfo{}, nil }
func (*shortDiagnosticFile) Close() error                   { return nil }

type zeroDiagnosticFileInfo struct{}

func (zeroDiagnosticFileInfo) Name() string       { return "diagnostics.jsonl" }
func (zeroDiagnosticFileInfo) Size() int64        { return 0 }
func (zeroDiagnosticFileInfo) Mode() os.FileMode  { return 0o600 }
func (zeroDiagnosticFileInfo) ModTime() time.Time { return time.Time{} }
func (zeroDiagnosticFileInfo) IsDir() bool        { return false }
func (zeroDiagnosticFileInfo) Sys() any           { return nil }
