package buildlogs

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ansiCSI = regexp.MustCompile("\\x1b\\[[0-?]*[ -/]*[@-~]")
	ansiOSC = regexp.MustCompile("\\x1b\\][^\\x07]*(?:\\x07|\\x1b\\\\)")
)

func (s *Service) readLines(reader io.Reader, source Source, redactions []string, options LogOptions, forceTimestamp bool, maxLines int64) ([]LogLine, int64, bool, error) {
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
	var emittedBytes, consumed int64
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
			line := s.safeLogLine(source, redactions, raw, options.Timestamps || forceTimestamp, lineTruncated || consumed > options.LimitBytes)
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

func (s *Service) safeLogLine(source Source, redactions []string, raw []byte, timestamps bool, truncated bool) LogLine {
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
	value = redactExactKubernetesIdentities(value, redactions)
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

func redactExactKubernetesIdentities(value string, identities []string) string {
	for _, identity := range identities {
		if len(identity) >= 8 {
			value = strings.ReplaceAll(value, identity, "[REDACTED KUBERNETES ID]")
		}
	}
	return value
}

func sanitizeText(value string) string {
	value = ansiOSC.ReplaceAllString(value, "")
	value = ansiCSI.ReplaceAllString(value, "")
	var builder strings.Builder
	for _, character := range value {
		if character == '\t' || character >= 0x20 && character != 0x7f {
			builder.WriteRune(character)
		}
	}
	return builder.String()
}

func truncateUTF8(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func lineFingerprint(source Source, timestamp time.Time, message string) string {
	digest := sha256.Sum256([]byte(source.ID + "\x00" + timestamp.UTC().Format(time.RFC3339Nano) + "\x00" + message))
	return hex.EncodeToString(digest[:])
}

func minInt64(left int64, right int) int {
	if left < int64(right) {
		return int(left)
	}
	return right
}
