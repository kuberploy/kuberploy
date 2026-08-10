package buildlogs

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

type followSession struct {
	service *Service
	request FollowRequest
	binding sourceBinding
	options LogOptions
	ctx     context.Context
	cancel  context.CancelFunc
	events  chan StreamEvent
	done    chan struct{}

	readerMu sync.Mutex
	reader   io.Closer
	wg       sync.WaitGroup
	bytes    atomic.Int64
	terminal atomic.Bool
	dedupe   *dedupeWindow
}

type dedupeWindow struct {
	mu    sync.Mutex
	limit int
	seen  map[string]struct{}
	order []string
}

func newDedupeWindow(limit int) *dedupeWindow {
	return &dedupeWindow{limit: limit, seen: make(map[string]struct{}, limit), order: make([]string, 0, limit)}
}

func (d *dedupeWindow) add(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, duplicate := d.seen[key]; duplicate {
		return false
	}
	d.seen[key] = struct{}{}
	d.order = append(d.order, key)
	if len(d.order) > d.limit {
		delete(d.seen, d.order[0])
		d.order = d.order[1:]
	}
	return true
}

func (s *Service) Follow(ctx context.Context, request FollowRequest) (*Stream, error) {
	options, err := s.normalizeOptions(request.Options, true)
	if err != nil {
		return nil, err
	}
	options.Timestamps = true
	setupCtx, setupCancel := context.WithTimeout(ctx, s.config.SnapshotTimeout)
	defer setupCancel()
	authorized, err := s.resolve(setupCtx, request.Access)
	if err != nil {
		return nil, err
	}
	if err = s.audit(setupCtx, request.Access, request.RequestID, "build.logs.follow"); err != nil {
		return nil, err
	}
	binding, err := s.discover(setupCtx, authorized, false)
	if err != nil {
		return nil, err
	}
	if request.Cursor != nil {
		cursor := request.Cursor
		if cursor.SourceID != binding.source.ID || !sourceIDPattern.MatchString(cursor.SourceID) || cursor.Timestamp.IsZero() ||
			!fingerprintPattern.MatchString(cursor.Fingerprint) || cursor.Timestamp.Before(s.now().Add(-s.config.MaxLookback)) ||
			cursor.Timestamp.After(s.now().Add(time.Minute)) {
			return nil, ErrInvalidRequest
		}
		cloned := *cursor
		cloned.Timestamp = cloned.Timestamp.UTC()
		request.Cursor = &cloned
	}
	sessionCtx, cancel := context.WithTimeout(ctx, s.config.MaxFollowDuration)
	session := &followSession{
		service: s, request: request, binding: binding, options: options, ctx: sessionCtx, cancel: cancel,
		events: make(chan StreamEvent, s.config.FollowBuffer), done: make(chan struct{}), dedupe: newDedupeWindow(s.config.DedupeEntries),
	}
	session.terminal.Store(isTerminalAttemptState(string(authorized.Attempt.State)))
	if request.Cursor != nil {
		session.dedupe.add(dedupeKey(request.Cursor.SourceID, request.Cursor.Timestamp, request.Cursor.Fingerprint))
	}
	session.wg.Add(1)
	go session.runSource()
	go session.run()
	return &Stream{Events: session.events, cancel: cancel, done: session.done}, nil
}

func (s *followSession) run() {
	defer close(s.done)
	revalidate := time.NewTicker(s.service.config.RevalidateInterval)
	heartbeat := time.NewTicker(s.service.config.HeartbeatInterval)
	rediscover := time.NewTicker(s.service.config.RediscoverInterval)
	defer revalidate.Stop()
	defer heartbeat.Stop()
	defer rediscover.Stop()
	for {
		select {
		case <-s.ctx.Done():
			s.closeReader()
			s.wg.Wait()
			if errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
				s.emitPriority(StreamEvent{Type: StreamTerminal, Terminal: &TerminalPayload{Code: "SessionExpired", Detail: safeTerminalDetail("SessionExpired")}, At: s.service.now()})
			}
			close(s.events)
			return
		case <-revalidate.C:
			if err := s.service.resolver.Revalidate(s.ctx, s.request.Access); err != nil {
				s.emitPriority(StreamEvent{Type: StreamTerminal, Terminal: &TerminalPayload{Code: "AuthorizationRevoked", Detail: safeTerminalDetail("AuthorizationRevoked")}, At: s.service.now()})
				s.cancel()
			}
		case <-heartbeat.C:
			s.tryEmit(StreamEvent{Type: StreamHeartbeat, At: s.service.now()})
		case <-rediscover.C:
			if err := s.rediscover(); err != nil {
				code := terminalCode(err)
				s.emitPriority(StreamEvent{Type: StreamTerminal, Terminal: &TerminalPayload{Code: code, Detail: safeTerminalDetail(code)}, At: s.service.now()})
				s.cancel()
			}
		}
	}
}

func (s *followSession) rediscover() error {
	authorized, err := s.service.resolve(s.ctx, s.request.Access)
	if err != nil {
		return err
	}
	if !sameBuildIdentity(authorized, s.binding.authorized) {
		return ErrGone
	}
	binding, err := s.service.discover(s.ctx, authorized, false)
	if err != nil {
		return err
	}
	if binding.job.UID != s.binding.job.UID || binding.pod.UID != s.binding.pod.UID || binding.source.ID != s.binding.source.ID {
		return ErrGone
	}
	s.terminal.Store(isTerminalAttemptState(string(authorized.Attempt.State)))
	return nil
}

func isTerminalAttemptState(state string) bool {
	return state == "succeeded" || state == "failed" || state == "cancelled"
}

func (s *followSession) runSource() {
	defer s.wg.Done()
	var lastTimestamp time.Time
	if s.request.Cursor != nil {
		lastTimestamp = s.request.Cursor.Timestamp
	}
	var dropped int64
	for {
		if s.ctx.Err() != nil {
			return
		}
		options := s.options
		if !lastTimestamp.IsZero() {
			since := lastTimestamp.Add(-s.service.config.ReconnectOverlap)
			if options.SinceTime == nil || since.After(*options.SinceTime) {
				options.SinceTime = &since
			}
		}
		remaining := s.service.config.MaxFollowBytes - s.bytes.Load()
		if remaining <= 0 {
			s.limitReached()
			return
		}
		if options.LimitBytes > remaining {
			options.LimitBytes = remaining
		}
		reader, err := s.service.open(s.ctx, s.binding, options)
		if err != nil {
			if errors.Is(err, ErrGone) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrScopeViolation) || errors.Is(err, ErrPreviousUnavailable) {
				code := terminalCode(err)
				s.emitPriority(StreamEvent{Type: StreamTerminal, Terminal: &TerminalPayload{Code: code, Detail: safeTerminalDetail(code)}, At: s.service.now()})
				s.cancel()
				return
			}
			s.emitStatus("reconnecting", "LogsUnavailable", &dropped)
			if !waitContext(s.ctx, s.service.config.ReconnectDelay) {
				return
			}
			continue
		}
		s.setReader(reader)
		s.emitStatus("active", "", &dropped)
		last, consumeErr := s.consume(reader, options, &dropped)
		s.clearReader(reader)
		_ = reader.Close()
		if last.After(lastTimestamp) {
			lastTimestamp = last
		}
		if s.ctx.Err() != nil {
			return
		}
		if errors.Is(consumeErr, ErrResponseLimitReached) {
			s.limitReached()
			return
		}
		if s.terminal.Load() {
			s.emitStatus("ended", "BuildCompleted", &dropped)
			s.emitPriority(StreamEvent{Type: StreamTerminal, Terminal: &TerminalPayload{Code: "BuildCompleted", Detail: safeTerminalDetail("BuildCompleted")}, At: s.service.now()})
			s.cancel()
			return
		}
		s.emitStatus("reconnecting", "StreamEnded", &dropped)
		if !waitContext(s.ctx, s.service.config.ReconnectDelay) {
			return
		}
	}
}

func (s *followSession) consume(reader io.Reader, options LogOptions, dropped *int64) (time.Time, error) {
	bufferSize := s.service.config.MaxLineBytes + 1
	if bufferSize > 64<<10 {
		bufferSize = 64 << 10
	}
	if bufferSize < 16 {
		bufferSize = 16
	}
	buffered := bufio.NewReaderSize(reader, bufferSize)
	var consumed int64
	var lastTimestamp time.Time
	for {
		if s.ctx.Err() != nil {
			return lastTimestamp, s.ctx.Err()
		}
		remainingOpen := options.LimitBytes - consumed
		remainingSession := s.service.config.MaxFollowBytes - s.bytes.Load()
		remaining := min(remainingOpen, remainingSession)
		if remaining <= 0 {
			return lastTimestamp, ErrResponseLimitReached
		}
		raw, truncated, rawBytes, err := readPhysicalLine(buffered, s.service.config.MaxLineBytes, remaining)
		consumed += rawBytes
		charged := rawBytes
		if charged > remaining {
			charged = remaining
		}
		if charged > 0 && !s.reserveBytes(charged) {
			return lastTimestamp, ErrResponseLimitReached
		}
		if rawBytes > 0 || len(raw) > 0 {
			line := s.service.safeLogLine(s.binding.source, s.binding.redactions, raw, true, truncated || rawBytes > remaining)
			if line.Timestamp == nil {
				now := s.service.now()
				line.Timestamp = &now
				fingerprint := lineFingerprint(line.Source, now, line.Message)
				line.Cursor = &LineCursor{SourceID: line.Source.ID, Timestamp: now, Fingerprint: fingerprint}
			}
			if line.Timestamp.After(lastTimestamp) {
				lastTimestamp = *line.Timestamp
			}
			key := dedupeKey(line.Source.ID, *line.Timestamp, line.Cursor.Fingerprint)
			if s.dedupe.add(key) {
				s.emitLine(line, dropped)
			}
		}
		if rawBytes > remaining {
			return lastTimestamp, ErrResponseLimitReached
		}
		if err != nil {
			return lastTimestamp, err
		}
	}
}

func dedupeKey(sourceID string, timestamp time.Time, fingerprint string) string {
	return sourceID + "\x00" + timestamp.UTC().Format(time.RFC3339Nano) + "\x00" + fingerprint
}

func (s *followSession) reserveBytes(count int64) bool {
	for {
		current := s.bytes.Load()
		if count < 0 || current > s.service.config.MaxFollowBytes-count {
			return false
		}
		if s.bytes.CompareAndSwap(current, current+count) {
			return true
		}
	}
}

func (s *followSession) emitLine(line LogLine, dropped *int64) {
	if *dropped > 0 {
		if s.tryEmit(StreamEvent{Type: StreamGap, Gap: &GapPayload{Source: line.Source, DroppedLines: *dropped}, At: s.service.now()}) {
			*dropped = 0
		} else {
			*dropped++
			return
		}
	}
	if !s.tryEmit(StreamEvent{Type: StreamLine, Line: &line, At: s.service.now()}) {
		*dropped++
	}
}

func (s *followSession) emitStatus(state, reason string, dropped *int64) {
	if *dropped > 0 && s.tryEmit(StreamEvent{Type: StreamGap, Gap: &GapPayload{Source: s.binding.source, DroppedLines: *dropped}, At: s.service.now()}) {
		*dropped = 0
	}
	status := StatusPayload{Source: s.binding.source, State: state, Reason: reason}
	s.tryEmit(StreamEvent{Type: StreamStatus, Status: &status, At: s.service.now()})
}

func (s *followSession) tryEmit(event StreamEvent) bool {
	select {
	case <-s.ctx.Done():
		return false
	case s.events <- event:
		return true
	default:
		return false
	}
}

func (s *followSession) emitPriority(event StreamEvent) {
	select {
	case s.events <- event:
		return
	default:
	}
	select {
	case <-s.events:
	default:
	}
	select {
	case s.events <- event:
	default:
	}
}

func (s *followSession) limitReached() {
	s.emitPriority(StreamEvent{Type: StreamTerminal, Terminal: &TerminalPayload{Code: "ResponseLimitReached", Detail: safeTerminalDetail("ResponseLimitReached")}, At: s.service.now()})
	s.cancel()
}

func (s *followSession) setReader(reader io.Closer) {
	s.readerMu.Lock()
	s.reader = reader
	s.readerMu.Unlock()
}

func (s *followSession) clearReader(reader io.Closer) {
	s.readerMu.Lock()
	if s.reader == reader {
		s.reader = nil
	}
	s.readerMu.Unlock()
}

func (s *followSession) closeReader() {
	s.readerMu.Lock()
	reader := s.reader
	s.reader = nil
	s.readerMu.Unlock()
	if reader != nil {
		_ = reader.Close()
	}
}

func terminalCode(err error) string {
	switch {
	case errors.Is(err, ErrUnauthorized):
		return "AuthorizationRevoked"
	case errors.Is(err, ErrGone), errors.Is(err, ErrNotFound), errors.Is(err, ErrScopeViolation):
		return "SourceReplacedOrDeleted"
	case errors.Is(err, ErrInsecureTransport):
		return "InsecureTransport"
	case errors.Is(err, ErrResponseLimitReached):
		return "ResponseLimitReached"
	default:
		return "LogsUnavailable"
	}
}

func safeTerminalDetail(code string) string {
	switch code {
	case "AuthorizationRevoked":
		return "Build log access is no longer authorized."
	case "SourceReplacedOrDeleted":
		return "The exact build log source was replaced or deleted."
	case "InsecureTransport":
		return "The Kubernetes log transport is not trusted."
	case "ResponseLimitReached":
		return "The bounded log byte limit was reached."
	case "BuildCompleted":
		return "The build log stream has ended."
	default:
		return "The bounded build log session ended."
	}
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
