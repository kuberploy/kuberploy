package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kuberploy/kuberploy/internal/builder"
)

const requestRoot = "/request"

type failureResult struct {
	APIVersion  string `json:"apiVersion"`
	OperationID string `json:"operationId,omitempty"`
	Generation  int64  `json:"generation,omitempty"`
	Status      string `json:"status"`
	Code        string `json:"code"`
	Message     string `json:"message"`
}

func main() {
	if os.Getenv("KUBERPLOY_ASKPASS") == "1" {
		prompt := ""
		if len(os.Args) > 1 {
			prompt = os.Args[1]
		}
		if err := builder.WriteAskpass(prompt, os.Stdout); err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	if err := run(os.Args[1:]); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: kuberploy-build-agent build|checkout --request PATH --result PATH")
	}
	requestPath, resultPath, err := parsePaths(args[1:])
	if err != nil {
		return err
	}
	requestFile, err := os.Open(requestPath)
	if err != nil {
		return errors.New("open request file")
	}
	defer requestFile.Close()
	// Source Dockerfiles are untrusted and can deliberately print mounted build
	// secrets. Keep tool output private; the bounded result exposes only typed
	// status/warnings and verified image metadata.
	executor := builder.OSExecutor{}
	switch args[0] {
	case "build":
		request, err := builder.DecodeBuildRequest(requestFile)
		if err != nil {
			return writeFailure(resultPath, "InvalidRequest", err, "", 0)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(request.Profile.TimeoutSeconds)*time.Second)
		defer cancel()
		agent := builder.NewAgent(executor)
		agent.Progress = os.Stdout
		result, err := agent.Run(ctx, request)
		if err != nil {
			return writeFailure(resultPath, "BuildFailed", err, request.OperationID, request.Generation)
		}
		return builder.WriteTerminationResultAtomic(resultPath, result)
	case "checkout":
		request, err := builder.DecodeCheckoutRequest(requestFile)
		if err != nil {
			return writeFailure(resultPath, "InvalidRequest", err, "", 0)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		result, err := builder.NewCheckout(executor).Run(ctx, request)
		if err != nil {
			return writeFailure(resultPath, "CheckoutFailed", err, request.OperationID, request.Generation)
		}
		return builder.WriteResultAtomic(resultPath, result)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func parsePaths(args []string) (string, string, error) {
	if len(args) != 4 {
		return "", "", errors.New("exactly --request PATH and --result PATH are required")
	}
	values := map[string]string{}
	for index := 0; index < len(args); index += 2 {
		name := args[index]
		if name != "--request" && name != "--result" {
			return "", "", fmt.Errorf("unknown argument %q", name)
		}
		if _, duplicate := values[name]; duplicate {
			return "", "", fmt.Errorf("duplicate argument %q", name)
		}
		values[name] = args[index+1]
	}
	requestPath := values["--request"]
	resultPath := values["--result"]
	if err := confinedFile(requestRoot, requestPath); err != nil {
		return "", "", fmt.Errorf("request path: %w", err)
	}
	if err := confinedFile(filepath.Dir(builder.DefaultBuildResult), resultPath); err != nil {
		return "", "", fmt.Errorf("result path: %w", err)
	}
	return requestPath, resultPath, nil
}

func confinedFile(root, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == root {
		return errors.New("must be a clean absolute file path")
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("must remain in its dedicated mount")
	}
	return nil
}

func writeFailure(path, code string, cause error, operationID string, generation int64) error {
	message := cause.Error()
	if len(message) > 512 {
		message = message[:512]
	}
	result := failureResult{
		APIVersion:  builder.ProtocolVersion,
		OperationID: operationID,
		Generation:  generation,
		Status:      "Failed",
		Code:        code,
		Message:     message,
	}
	if err := builder.WriteTerminationResultAtomic(path, result); err != nil {
		return errors.New("operation failed and its bounded result could not be written")
	}
	encoded, _ := json.Marshal(result)
	_, _ = io.Discard.Write(encoded)
	return cause
}
