package runtimeview

import (
	"bufio"
	"context"
	"errors"
	"io"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

var sourceIDPattern = regexp.MustCompile(`^src_[a-f0-9]{32}$`)

type followSession struct {
	service *Service
	request FollowRequest
	target  AuthorizedTarget
	options LogOptions
	ctx     context.Context
	cancel  context.CancelFunc
	events  chan StreamEvent
	done    chan struct{}

	mu      sync.Mutex
	sources map[string]*sourceRunner
	cursors map[string]ReconnectCursor
	wg      sync.WaitGroup
	bytes   atomic.Int64
	dedupe  *dedupeWindow
}

type sourceRunner struct {
	binding sourceBinding
	mu      sync.Mutex
	reader  io.Closer
}

func (r *sourceRunner) setReader(reader io.Closer) {
	r.mu.Lock()
	r.reader = reader
	r.mu.Unlock()
}

func (r *sourceRunner) clearReader(reader io.Closer) {
	r.mu.Lock()
	if r.reader == reader {
		r.reader = nil
	}
	r.mu.Unlock()
}

func (r *sourceRunner) closeReader() {
	r.mu.Lock()
	reader := r.reader
	r.reader = nil
	r.mu.Unlock()
	if reader != nil {
		_ = reader.Close()
	}
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
	if options.Previous {
		return nil, ErrInvalidRequest
	}
	if len(request.Cursors) > s.config.MaxSources {
		return nil, ErrInvalidRequest
	}
	cursors := make(map[string]ReconnectCursor, len(request.Cursors))
	for _, cursor := range request.Cursors {
		if !sourceIDPattern.MatchString(cursor.SourceID) || cursor.Timestamp.IsZero() ||
			!fingerprintPattern.MatchString(cursor.Fingerprint) || cursor.Timestamp.Before(s.now().Add(-s.config.MaxLookback)) ||
			cursor.Timestamp.After(s.now().Add(time.Minute)) {
			return nil, ErrInvalidRequest
		}
		if _, duplicate := cursors[cursor.SourceID]; duplicate {
			return nil, ErrInvalidRequest
		}
		cursor.Timestamp = cursor.Timestamp.UTC()
		cursors[cursor.SourceID] = cursor
	}
	target, err := s.resolve(ctx, request.Target)
	if err != nil {
		return nil, err
	}
	graph, err := s.discoverGraph(ctx, target)
	if err != nil {
		return nil, err
	}
	sources, err := s.sources(graph, options)
	if err != nil {
		return nil, err
	}
	knownSources := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		knownSources[source.source.ID] = struct{}{}
	}
	for sourceID := range cursors {
		if _, allowed := knownSources[sourceID]; !allowed {
			return nil, ErrInvalidRequest
		}
	}
	sessionCtx, cancel := context.WithTimeout(ctx, s.config.MaxFollowDuration)
	events := make(chan StreamEvent, s.config.FollowBuffer)
	done := make(chan struct{})
	session := &followSession{
		service: s,
		request: request,
		target:  target,
		options: options,
		ctx:     sessionCtx,
		cancel:  cancel,
		events:  events,
		done:    done,
		sources: make(map[string]*sourceRunner),
		cursors: cursors,
		dedupe:  newDedupeWindow(s.config.DedupeEntries),
	}
	for _, cursor := range cursors {
		session.dedupe.add(dedupeKey(cursor.SourceID, cursor.Timestamp, cursor.Fingerprint))
	}
	for _, source := range sources {
		session.startSource(source)
	}
	go session.run()
	return &Stream{Events: events, cancel: cancel, done: done}, nil
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
			s.closeReaders()
			s.wg.Wait()
			if errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
				s.emitPriority(StreamEvent{Type: StreamTerminal, Terminal: &StreamTerminalPayload{Code: "SessionExpired", Detail: safeTerminalDetail("SessionExpired")}, At: s.service.now()})
			}
			close(s.events)
			return
		case <-revalidate.C:
			if err := s.service.resolver.Revalidate(s.ctx, s.request.Target); err != nil {
				s.emitPriority(StreamEvent{Type: StreamTerminal, Terminal: &StreamTerminalPayload{Code: "AuthorizationRevoked", Detail: safeTerminalDetail("AuthorizationRevoked")}, At: s.service.now()})
				s.cancel()
			}
		case <-heartbeat.C:
			s.tryEmit(StreamEvent{Type: StreamHeartbeat, At: s.service.now()})
		case <-rediscover.C:
			if err := s.rediscover(); err != nil {
				code := terminalCode(err)
				s.emitPriority(StreamEvent{Type: StreamTerminal, Terminal: &StreamTerminalPayload{Code: code, Detail: safeTerminalDetail(code)}, At: s.service.now()})
				s.cancel()
			}
		}
	}
}

func (s *followSession) rediscover() error {
	target, err := s.service.resolve(s.ctx, s.request.Target)
	if err != nil {
		return err
	}
	if target.Namespace != s.target.Namespace || target.ApplicationID != s.target.ApplicationID || !sameDeploymentRefs(target.Deployments, s.target.Deployments) {
		return ErrGone
	}
	graph, err := s.service.discoverGraph(s.ctx, target)
	if err != nil {
		return err
	}
	sources, err := s.service.sources(graph, s.options)
	if err != nil {
		return err
	}
	for _, source := range sources {
		s.startSource(source)
	}
	return nil
}

func sameDeploymentRefs(left, right []DeploymentRef) bool {
	if len(left) != len(right) {
		return false
	}
	set := make(map[DeploymentRef]struct{}, len(left))
	for _, value := range left {
		set[value] = struct{}{}
	}
	for _, value := range right {
		if _, ok := set[value]; !ok {
			return false
		}
	}
	return true
}

func (s *followSession) startSource(binding sourceBinding) {
	s.mu.Lock()
	if _, exists := s.sources[binding.source.ID]; exists || s.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	runner := &sourceRunner{binding: binding}
	s.sources[binding.source.ID] = runner
	s.wg.Add(1)
	s.mu.Unlock()
	go s.runSource(runner)
}

func (s *followSession) runSource(runner *sourceRunner) {
	defer s.wg.Done()
	var lastTimestamp time.Time
	if cursor, ok := s.cursors[runner.binding.source.ID]; ok {
		lastTimestamp = cursor.Timestamp
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
		reader, err := s.service.openSource(s.ctx, runner.binding, options, true)
		if err != nil {
			if errors.Is(err, ErrGone) || errors.Is(err, ErrNotFound) {
				s.emitSourceStatus(runner.binding.source, "ended", "PodReplacedOrDeleted", &dropped)
				return
			}
			if errors.Is(err, ErrScopeViolation) || errors.Is(err, ErrContainerNotFound) || errors.Is(err, ErrPreviousUnavailable) {
				code := terminalCode(err)
				s.emitPriority(StreamEvent{Type: StreamTerminal, Terminal: &StreamTerminalPayload{Code: code, Detail: safeTerminalDetail(code)}, At: s.service.now()})
				s.cancel()
				return
			}
			s.emitSourceStatus(runner.binding.source, "reconnecting", "LogsUnavailable", &dropped)
			if !waitContext(s.ctx, s.service.config.ReconnectDelay) {
				return
			}
			continue
		}
		runner.setReader(reader)
		s.emitSourceStatus(runner.binding.source, "active", "", &dropped)
		last, consumeErr := s.consume(runner, reader, options, &dropped)
		runner.clearReader(reader)
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
		s.emitSourceStatus(runner.binding.source, "reconnecting", "StreamEnded", &dropped)
		if !waitContext(s.ctx, s.service.config.ReconnectDelay) {
			return
		}
	}
}

func (s *followSession) consume(runner *sourceRunner, reader io.Reader, options LogOptions, dropped *int64) (time.Time, error) {
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
		if rawBytes > 0 || len(raw) > 0 {
			line := s.service.safeLogLine(runner.binding.source, raw, true, truncated || rawBytes > remaining)
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
				if !s.reserveBytes(int64(len(line.Message))) {
					return lastTimestamp, ErrResponseLimitReached
				}
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
		gap := StreamEvent{Type: StreamGap, Gap: &StreamGapPayload{Source: line.Source, DroppedLines: *dropped}, At: s.service.now()}
		if s.tryEmit(gap) {
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

func (s *followSession) emitSourceStatus(source LogSource, state, reason string, dropped *int64) {
	if *dropped > 0 {
		if s.tryEmit(StreamEvent{Type: StreamGap, Gap: &StreamGapPayload{Source: source, DroppedLines: *dropped}, At: s.service.now()}) {
			*dropped = 0
		}
	}
	status := SourceStatus{Source: source, State: state, Reason: reason}
	s.tryEmit(StreamEvent{Type: StreamSourceStatus, Status: &status, At: s.service.now()})
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
	// A terminal authorization/session event is more important than one queued
	// line. Evict at most one bounded item; the terminal status explicitly tells
	// the client that the stream ended and no continuity is claimed.
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
	s.emitPriority(StreamEvent{Type: StreamTerminal, Terminal: &StreamTerminalPayload{Code: "ResponseLimitReached", Detail: "The bounded log byte limit was reached."}, At: s.service.now()})
	s.cancel()
}

func (s *followSession) closeReaders() {
	s.mu.Lock()
	runners := make([]*sourceRunner, 0, len(s.sources))
	for _, runner := range s.sources {
		runners = append(runners, runner)
	}
	s.mu.Unlock()
	for _, runner := range runners {
		runner.closeReader()
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
