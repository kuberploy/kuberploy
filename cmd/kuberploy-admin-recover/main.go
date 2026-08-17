package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/kuberploy/kuberploy/internal/emailaddr"
	"github.com/kuberploy/kuberploy/internal/passwordauth"
	"github.com/kuberploy/kuberploy/internal/store/postgres"
)

const confirmationPrefix = "recover-email:"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "kuberploy admin recovery failed:", err)
		os.Exit(1)
	}
	fmt.Println("Kuberploy legacy administrator email recovered; existing sessions were revoked.")
}

func run(ctx context.Context) error {
	databaseURL, err := requiredEnv("KUBERPLOY_DATABASE_URL")
	if err != nil {
		return err
	}
	userID, err := requiredEnv("KUBERPLOY_RECOVERY_USER_ID")
	if err != nil {
		return err
	}
	confirmation, err := requiredEnv("KUBERPLOY_RECOVERY_CONFIRM")
	if err != nil {
		return err
	}
	if confirmation != confirmationPrefix+userID {
		return errors.New("KUBERPLOY_RECOVERY_CONFIRM must equal recover-email:<exact-user-id>")
	}
	emailPath, err := requiredEnv("KUBERPLOY_RECOVERY_EMAIL_FILE")
	if err != nil {
		return err
	}
	passwordPath, err := requiredEnv("KUBERPLOY_RECOVERY_PASSWORD_FILE")
	if err != nil {
		return err
	}
	emailRaw, err := readSecretFile(emailPath, "email")
	if err != nil {
		return err
	}
	email, ok := emailaddr.Normalize(emailRaw)
	if !ok {
		return errors.New("recovery email is invalid")
	}
	password, err := readSecretFile(passwordPath, "password")
	if err != nil {
		return err
	}
	passwordHash, err := passwordauth.Hash(password)
	if err != nil {
		return errors.New("recovery password is invalid")
	}
	store, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	return store.RecoverLegacyLocalAdmin(ctx, userID, email, passwordHash, "offline-email-recovery:"+userID)
}

func requiredEnv(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func readSecretFile(path, label string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("recovery %s file must be an absolute path", label)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("stat recovery %s file: %w", label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("recovery %s file must be one regular non-symlink file", label)
	}
	if info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("recovery %s file must use mode 0600", label)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read recovery %s file: %w", label, err)
	}
	value := strings.TrimSuffix(string(data), "\n")
	value = strings.TrimSuffix(value, "\r")
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("recovery %s file must contain one non-empty line", label)
	}
	return value, nil
}
