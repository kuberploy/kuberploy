package builder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
)

const maxCommandOutputBytes = 256 << 10

// Invocation is an argv-only process call. Callers never construct a shell
// command, and secret material is permitted only through Stdin.
type Invocation struct {
	Argv  []string
	Env   []string
	Stdin []byte
}

type CommandResult struct {
	Output    string
	Stdout    string
	Stderr    string
	Truncated bool
}

type CommandExecutor interface {
	Execute(context.Context, Invocation) (CommandResult, error)
}

type OSExecutor struct {
	Log io.Writer
}

func (e OSExecutor) Execute(ctx context.Context, invocation Invocation) (CommandResult, error) {
	if len(invocation.Argv) == 0 || invocation.Argv[0] == "" {
		return CommandResult{}, errors.New("empty command")
	}
	command := exec.CommandContext(ctx, invocation.Argv[0], invocation.Argv[1:]...)
	command.Env = sanitizedEnvironment(invocation.Env)
	command.Stdin = bytes.NewReader(invocation.Stdin)
	stdout := newBoundedBuffer(maxCommandOutputBytes / 2)
	stderr := newBoundedBuffer(maxCommandOutputBytes / 2)
	if e.Log != nil {
		command.Stdout = io.MultiWriter(e.Log, stdout)
		command.Stderr = io.MultiWriter(e.Log, stderr)
	} else {
		command.Stdout = stdout
		command.Stderr = stderr
	}
	err := command.Run()
	combined := stdout.String()
	if stderr.Len() > 0 {
		combined += "\n" + stderr.String()
	}
	return CommandResult{Output: combined, Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.truncated || stderr.truncated}, err
}

func sanitizedEnvironment(injected []string) []string {
	overrides := map[string]struct{}{}
	for _, entry := range injected {
		if key, _, found := strings.Cut(entry, "="); found {
			overrides[key] = struct{}{}
		}
	}
	result := make([]string, 0, len(os.Environ())+len(injected))
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, overridden := overrides[key]; overridden || !allowedInheritedEnvironment(key) {
			continue
		}
		result = append(result, entry)
	}
	return append(result, injected...)
}

func allowedInheritedEnvironment(key string) bool {
	switch key {
	case "PATH", "SSL_CERT_FILE", "SSL_CERT_DIR", "LANG", "LC_ALL", "TZ":
		return true
	default:
		return false
	}
}

type boundedBuffer struct {
	bytes.Buffer
	remaining int
	truncated bool
}

func newBoundedBuffer(limit int) *boundedBuffer { return &boundedBuffer{remaining: limit} }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	if b.remaining == 0 {
		b.truncated = b.truncated || original > 0
		return original, nil
	}
	portion := p
	if len(portion) > b.remaining {
		portion = portion[:b.remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(portion)
	b.remaining -= len(portion)
	return original, nil
}

func cloneInvocation(invocation Invocation) Invocation {
	return Invocation{
		Argv:  slices.Clone(invocation.Argv),
		Env:   slices.Clone(invocation.Env),
		Stdin: slices.Clone(invocation.Stdin),
	}
}

func commandError(step string, err error) error {
	if err == nil {
		return nil
	}
	// Process output is deliberately excluded: source builds may be hostile and
	// credential-bearing tools can echo sensitive diagnostics.
	return fmt.Errorf("%s failed: %w", step, err)
}
