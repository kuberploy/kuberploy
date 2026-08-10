package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kuberploy/kuberploy/internal/upgrade"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The exact manifest is base64 encoded by JSON, so allow its bounded 256 KiB
	// payload plus encoding and closed-protocol overhead.
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, (384<<10)+1))
	decoder.DisallowUnknownFields()
	var request upgrade.ExecutableRequest
	if err := decoder.Decode(&request); err != nil {
		return fmt.Errorf("decode closed upgrade request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("upgrade request must contain exactly one JSON object")
	}
	jobs, podNamespace, err := upgrade.NewInClusterJobAPI()
	if err != nil {
		return err
	}
	namespace := env("KUBERPLOY_UPGRADER_NAMESPACE", podNamespace)
	if namespace != podNamespace {
		return errors.New("configured upgrade namespace does not match the runner pod namespace")
	}
	deadline, err := strconv.ParseInt(env("KUBERPLOY_UPGRADER_ACTIVE_DEADLINE_SECONDS", "900"), 10, 64)
	if err != nil {
		return errors.New("KUBERPLOY_UPGRADER_ACTIVE_DEADLINE_SECONDS must be an integer")
	}
	runner := upgrade.KubernetesRunner{
		Jobs:                  jobs,
		Namespace:             namespace,
		ServiceAccount:        env("KUBERPLOY_UPGRADER_SERVICE_ACCOUNT", "kuberploy-upgrade"),
		ReleaseName:           env("KUBERPLOY_UPGRADER_RELEASE_NAME", "kuberploy"),
		ActiveDeadlineSeconds: deadline,
		PollInterval:          2 * time.Second,
	}
	result, err := runner.Run(ctx, request)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
