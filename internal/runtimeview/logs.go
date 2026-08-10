package runtimeview

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type defenseInDepthRedactor struct {
	mu           sync.Mutex
	inPrivateKey bool
}

var (
	ansiCSI                 = regexp.MustCompile("\\x1b\\[[0-?]*[ -/]*[@-~]")
	ansiOSC                 = regexp.MustCompile("\\x1b\\][^\\x07]*(?:\\x07|\\x1b\\\\)")
	authorizationPattern    = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*(?:bearer\s+)?)([^\s,;]+)`)
	secretAssignmentPattern = regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key|access[_-]?key)(\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;]+)`)
	kuberployTokenPattern   = regexp.MustCompile(`\bkp_sa_[A-Za-z0-9_-]{20,}\b`)
	githubTokenPattern      = regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)
	jwtPattern              = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
	awsAccessKeyPattern     = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
)

// NewDefenseInDepthRedactor masks common credential shapes without reading
// Kubernetes Secrets. It is intentionally documented as a guard, not a
// promise that arbitrary application output can always be made non-sensitive.
func NewDefenseInDepthRedactor() Redactor { return &defenseInDepthRedactor{} }

func (r *defenseInDepthRedactor) Redact(value string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	upper := strings.ToUpper(value)
	if strings.Contains(upper, "-----BEGIN ") && strings.Contains(upper, "PRIVATE KEY-----") {
		r.inPrivateKey = true
		return "[REDACTED PRIVATE KEY]"
	}
	if r.inPrivateKey {
		if strings.Contains(upper, "-----END ") && strings.Contains(upper, "PRIVATE KEY-----") {
			r.inPrivateKey = false
		}
		return "[REDACTED PRIVATE KEY]"
	}
	value = authorizationPattern.ReplaceAllString(value, `${1}[REDACTED]`)
	value = secretAssignmentPattern.ReplaceAllString(value, `${1}${2}[REDACTED]`)
	value = kuberployTokenPattern.ReplaceAllString(value, "[REDACTED]")
	value = githubTokenPattern.ReplaceAllString(value, "[REDACTED]")
	value = jwtPattern.ReplaceAllString(value, "[REDACTED]")
	return awsAccessKeyPattern.ReplaceAllString(value, "[REDACTED]")
}

func (s *Service) Snapshot(ctx context.Context, request SnapshotRequest) (LogSnapshot, error) {
	options, err := s.normalizeOptions(request.Options, false)
	if err != nil {
		return LogSnapshot{}, err
	}
	target, err := s.resolve(ctx, request.Target)
	if err != nil {
		return LogSnapshot{}, err
	}
	graph, err := s.discoverGraph(ctx, target)
	if err != nil {
		return LogSnapshot{}, err
	}
	sources, err := s.sources(graph, options)
	if err != nil {
		return LogSnapshot{}, err
	}
	snapshot := LogSnapshot{Lines: []LogLine{}, Sources: make([]LogSource, 0, len(sources)), Statuses: []SourceStatus{}, ObservedAt: s.now()}
	for _, binding := range sources {
		if snapshot.Bytes >= s.config.MaxSnapshotBytes {
			snapshot.Truncated = true
			break
		}
		snapshot.Sources = append(snapshot.Sources, binding.source)
		sourceOptions := options
		if remaining := s.config.MaxSnapshotBytes - snapshot.Bytes; sourceOptions.LimitBytes > remaining {
			sourceOptions.LimitBytes = remaining
		}
		reader, openErr := s.openSource(ctx, binding, sourceOptions, false)
		if openErr != nil {
			if errors.Is(openErr, ErrScopeViolation) || errors.Is(openErr, ErrContainerNotFound) ||
				errors.Is(openErr, ErrContainerRequired) || errors.Is(openErr, ErrPreviousUnavailable) ||
				errors.Is(openErr, ErrInsecureTransport) {
				return LogSnapshot{}, openErr
			}
			state, reason := "error", "LogsUnavailable"
			if errors.Is(openErr, ErrGone) || errors.Is(openErr, ErrNotFound) {
				state, reason = "expired", "PodReplacedOrDeleted"
			}
			snapshot.Statuses = append(snapshot.Statuses, SourceStatus{Source: binding.source, State: state, Reason: reason})
			continue
		}
		lines, bytesRead, truncated, readErr := s.readLines(reader, binding.source, sourceOptions, false, sourceOptions.TailLines)
		_ = reader.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			snapshot.Statuses = append(snapshot.Statuses, SourceStatus{Source: binding.source, State: "error", Reason: "LogsUnavailable"})
		}
		snapshot.Lines = append(snapshot.Lines, lines...)
		snapshot.Bytes += bytesRead
		snapshot.Truncated = snapshot.Truncated || truncated
	}
	// Kubernetes preserves order per source. This merge is explicitly only
	// best-effort chronological when timestamps are present.
	sort.SliceStable(snapshot.Lines, func(i, j int) bool {
		left, right := snapshot.Lines[i].Timestamp, snapshot.Lines[j].Timestamp
		if left == nil || right == nil || left.Equal(*right) {
			return false
		}
		return left.Before(*right)
	})
	return snapshot, nil
}

func (s *Service) readLines(reader io.Reader, source LogSource, options LogOptions, forceTimestamp bool, maxLines int64) ([]LogLine, int64, bool, error) {
	limited := &io.LimitedReader{R: reader, N: options.LimitBytes + 1}
	bufferSize := s.config.MaxLineBytes + 1
	if bufferSize > 64<<10 {
		bufferSize = 64 << 10
	}
	if bufferSize < 16 {
		bufferSize = 16
	}
	buffered := bufio.NewReaderSize(limited, bufferSize)
	lines := make([]LogLine, 0, minInt64(maxLines, 256))
	var emittedBytes int64
	var consumed int64
	truncatedBody := false
	for {
		if int64(len(lines)) >= maxLines {
			if _, peekErr := buffered.Peek(1); peekErr == nil {
				truncatedBody = true
			}
			break
		}
		allowed := options.LimitBytes - consumed
		if allowed < 0 {
			truncatedBody = true
			break
		}
		raw, lineTruncated, rawBytes, readErr := readPhysicalLine(buffered, s.config.MaxLineBytes, allowed)
		consumed += rawBytes
		if consumed > options.LimitBytes {
			truncatedBody = true
		}
		if rawBytes > 0 || len(raw) > 0 {
			line := s.safeLogLine(source, raw, options.Timestamps || forceTimestamp, lineTruncated || consumed > options.LimitBytes)
			emittedBytes += int64(len(line.Message))
			lines = append(lines, line)
		}
		if consumed > options.LimitBytes {
			break
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return lines, emittedBytes, truncatedBody, nil
			}
			return lines, emittedBytes, truncatedBody, readErr
		}
	}
	return lines, emittedBytes, truncatedBody, nil
}

func readPhysicalLine(reader *bufio.Reader, maxBytes int, allowed int64) ([]byte, bool, int64, error) {
	var output []byte
	truncated := false
	var readBytes int64
	for {
		fragment, err := reader.ReadSlice('\n')
		readBytes += int64(len(fragment))
		keep := len(fragment)
		if allowed < readBytes {
			over := readBytes - allowed
			if over >= int64(keep) {
				keep = 0
			} else {
				keep -= int(over)
			}
			truncated = true
		}
		if remaining := maxBytes - len(output); keep > remaining {
			keep = max(remaining, 0)
			truncated = true
		}
		if keep > 0 {
			output = append(output, fragment[:keep]...)
		}
		if err == nil {
			output = bytes.TrimSuffix(output, []byte{'\n'})
			output = bytes.TrimSuffix(output, []byte{'\r'})
			return output, truncated, readBytes, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		output = bytes.TrimSuffix(output, []byte{'\r'})
		return output, truncated, readBytes, err
	}
}

func (s *Service) safeLogLine(source LogSource, raw []byte, timestamps bool, truncated bool) LogLine {
	value := strings.ToValidUTF8(string(raw), "�")
	var timestamp *time.Time
	if timestamps {
		if separator := strings.IndexByte(value, ' '); separator > 0 {
			if parsed, err := time.Parse(time.RFC3339Nano, value[:separator]); err == nil {
				parsed = parsed.UTC()
				timestamp = &parsed
				value = value[separator+1:]
			}
		}
	}
	value = sanitizeText(value)
	value = sanitizeText(s.redactor.Redact(value))
	if len(value) > s.config.MaxLineBytes {
		value = truncateUTF8(value, s.config.MaxLineBytes)
		truncated = true
	}
	line := LogLine{Type: "line", Timestamp: timestamp, Source: source, Message: value, Truncated: truncated}
	if timestamp != nil {
		fingerprint := lineFingerprint(source, *timestamp, value)
		line.Cursor = &LineCursor{SourceID: source.ID, Timestamp: *timestamp, Fingerprint: fingerprint}
	}
	return line
}

func sanitizeText(value string) string {
	value = ansiOSC.ReplaceAllString(value, "")
	value = ansiCSI.ReplaceAllString(value, "")
	var builder strings.Builder
	builder.Grow(len(value))
	for _, character := range value {
		if character == '\t' || character >= 0x20 && character != 0x7f {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func truncateUTF8(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

func lineFingerprint(source LogSource, timestamp time.Time, message string) string {
	digest := sha256.Sum256([]byte(source.ID + "\x00" + timestamp.UTC().Format(time.RFC3339Nano) + "\x00" + message))
	return hex.EncodeToString(digest[:])
}

func minInt64(value int64, maximum int) int {
	if value < int64(maximum) {
		return int(value)
	}
	return maximum
}
