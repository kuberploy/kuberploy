package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/kuberploy/kuberploy/internal/bootstrapsecret"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := bootstrapsecret.ConfigFromEnvironment()
	if err != nil {
		slog.Error("bootstrap token generator stopped", "error", err)
		os.Exit(1)
	}
	result, err := bootstrapsecret.Generate(ctx, cfg)
	if err != nil {
		slog.Error("bootstrap token generator stopped", "error", err)
		os.Exit(1)
	}
	if !result.Created {
		fmt.Println("Kuberploy bootstrap Secret already exists; no token was disclosed.")
		return
	}
	fmt.Printf("KUBERPLOY_BOOTSTRAP_TOKEN=%s\n", result.Token)
}
