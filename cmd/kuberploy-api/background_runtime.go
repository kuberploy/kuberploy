package main

import (
	"context"
	"log/slog"
	"time"
)

const backgroundRuntimeRestartDelay = time.Second

func superviseBackgroundRuntime(ctx context.Context, name string, restartDelay time.Duration, run func(context.Context) error) {
	if ctx == nil || name == "" || restartDelay <= 0 || run == nil {
		return
	}
	for {
		err := run(ctx)
		if ctx.Err() != nil {
			return
		}
		slog.Error("background runtime stopped; restarting", "runtime", name, "error", err)
		timer := time.NewTimer(restartDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
}
