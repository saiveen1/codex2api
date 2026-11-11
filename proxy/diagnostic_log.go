package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/codex2api/security"
)

const (
	diagnosticLogFileName = "error_diagnostics.jsonl"
	diagnosticMaxLineSize = 32 * 1024
	diagnosticMaxFileSize = 10 * 1024 * 1024
	diagnosticBackupCount = 3
)

var errDiagnosticLineTooLarge = errors.New("diagnostic log line exceeds size limit")

// diagnosticFileLogger is a small, dependency-free JSONL file sink. Its mutex
// serializes all file lifecycle operations so a record cannot be interleaved
// with another record or a rotation.
type diagnosticFileLogger struct {
	mu sync.Mutex

	path     string
	maxBytes int64
	backups  int
	file     diagnosticLogFile

	// rotateBeforeWrite is set after a partial or otherwise uncertain write so
	// later records are not appended to a potentially malformed JSONL file.
	rotateBeforeWrite bool
	failureReported   bool
	sink              diagnosticWarningSink
}

type diagnosticLogFile interface {
	io.Writer
	Stat() (os.FileInfo, error)
	Close() error
}

type diagnosticWarningSink interface {
	WarnFailure()
	WarnRecovered()
}

type stderrDiagnosticWarningSink struct{}

func (stderrDiagnosticWarningSink) WarnFailure() {
	// Do not include event data, errors, or paths: each could carry secrets.
	_, _ = fmt.Fprintln(os.Stderr, "diagnostic log write failed")
}

func (stderrDiagnosticWarningSink) WarnRecovered() {
	_, _ = fmt.Fprintln(os.Stderr, "diagnostic log writing recovered")
}

var diagnosticLogger = &diagnosticFileLogger{
	maxBytes: diagnosticMaxFileSize,
	backups:  diagnosticBackupCount,
}

// writeDiagnostic appends one JSON object/value as a single JSONL record.
// File logging can be disabled globally, in which case it performs no disk I/O.
func writeDiagnostic(event any) {
	if security.FileLogsDisabled() {
		return
	}

	data, err := marshalDiagnosticLine(event)
	if err != nil {
		diagnosticLogger.recordFailure()
		return
	}
	_ = diagnosticLogger.write(data)
}

// closeDiagnosticLog releases the diagnostic log file. A later write reopens it.
func closeDiagnosticLog() {
	_ = diagnosticLogger.close()
}

func marshalDiagnosticLine(event any) ([]byte, error) {
	data, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if len(data) > diagnosticMaxLineSize {
		return nil, errDiagnosticLineTooLarge
	}
	return data, nil
}

func (l *diagnosticFileLogger) write(data []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.path == "" {
		l.path = filepath.Join(errorLogDir(), diagnosticLogFileName)
	}

	if err := l.validateLocked(data); err != nil {
		return l.failLocked(err)
	}

	if l.rotateBeforeWrite {
		if err := l.rotateLocked(); err != nil {
			return l.failLocked(err)
		}
	} else {
		if err := l.openLocked(); err != nil {
			return l.failLocked(err)
		}
		info, err := l.file.Stat()
		if err != nil {
			return l.failLocked(err)
		}
		if info.Size() > l.maxBytes-int64(len(data)) {
			if err := l.rotateLocked(); err != nil {
				return l.failLocked(err)
			}
		}
	}

	if err := l.openLocked(); err != nil {
		return l.failLocked(err)
	}

	n, err := l.file.Write(data)
	if err != nil || n != len(data) {
		l.rotateBeforeWrite = true
		if err == nil {
			err = io.ErrShortWrite
		}
		return l.failLocked(err)
	}

	l.rotateBeforeWrite = false
	l.recoverLocked()
	return nil
}

func (l *diagnosticFileLogger) close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	if err := l.closeFileLocked(); err != nil {
		return l.failLocked(err)
	}
	return nil
}

func (l *diagnosticFileLogger) validateLocked(data []byte) error {
	if l.path == "" {
		return errors.New("diagnostic log path is empty")
	}
	if l.maxBytes <= 0 {
		return errors.New("diagnostic log maximum size is invalid")
	}
	if l.backups < 0 {
		return errors.New("diagnostic log backup count is invalid")
	}
	if int64(len(data)) > l.maxBytes {
		return errors.New("diagnostic log record exceeds file size limit")
	}
	return nil
}

func (l *diagnosticFileLogger) openLocked() error {
	if l.file != nil {
		return nil
	}

	dir := filepath.Dir(l.path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}

	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	l.file = file
	return nil
}

func (l *diagnosticFileLogger) rotateLocked() error {
	if err := l.closeFileLocked(); err != nil {
		return err
	}

	if l.backups == 0 {
		if err := removeDiagnosticFileIfExists(l.path); err != nil {
			return err
		}
		l.rotateBeforeWrite = false
		return nil
	}

	oldest := diagnosticBackupPath(l.path, l.backups)
	if err := removeDiagnosticFileIfExists(oldest); err != nil {
		return err
	}

	// Remove the oldest backup first, then move each source into the now-empty
	// destination. This ordering works on Windows, where Rename does not replace
	// an existing destination.
	for index := l.backups - 1; index >= 1; index-- {
		if err := renameDiagnosticFileIfExists(
			diagnosticBackupPath(l.path, index),
			diagnosticBackupPath(l.path, index+1),
		); err != nil {
			return err
		}
	}
	if err := renameDiagnosticFileIfExists(l.path, diagnosticBackupPath(l.path, 1)); err != nil {
		return err
	}

	l.rotateBeforeWrite = false
	return nil
}

func (l *diagnosticFileLogger) closeFileLocked() error {
	if l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	return file.Close()
}

func (l *diagnosticFileLogger) failLocked(err error) error {
	if !l.failureReported {
		// Keep diagnostics about the sink generic: errors and paths can expose
		// environment details, while the event itself may contain secrets.
		if l.sink == nil {
			l.sink = stderrDiagnosticWarningSink{}
		}
		l.sink.WarnFailure()
		l.failureReported = true
	}
	return err
}

func (l *diagnosticFileLogger) recordFailure() {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.failLocked(errors.New("diagnostic log record rejected"))
}

func (l *diagnosticFileLogger) recoverLocked() {
	if !l.failureReported {
		return
	}
	if l.sink == nil {
		l.sink = stderrDiagnosticWarningSink{}
	}
	l.sink.WarnRecovered()
	l.failureReported = false
}

func diagnosticBackupPath(path string, index int) string {
	return fmt.Sprintf("%s.%d", path, index)
}

func removeDiagnosticFileIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func renameDiagnosticFileIfExists(oldPath, newPath string) error {
	if err := os.Rename(oldPath, newPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
