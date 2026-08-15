package helmapps

import (
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestClassifyPostgresUsesBoundedTriggerDiagnostic(t *testing.T) {
	databaseErr := &pgconn.PgError{Code: "23514", Message: "private row detail"}
	classified := classifyPostgres(databaseErr)
	if !errors.Is(classified, ErrConflict) {
		t.Fatalf("classified error = %v, want ErrConflict", classified)
	}
	if got := classified.Error(); got != ErrConflict.Error()+": database rejected operation (SQLSTATE 23514)" {
		t.Fatalf("classified error = %q", got)
	}
	if strings.Contains(classified.Error(), databaseErr.Message) {
		t.Fatal("database message leaked through bounded conflict diagnostic")
	}

	constrained := classifyPostgres(&pgconn.PgError{Code: "23505", ConstraintName: "safe_unique_key"})
	if got := constrained.Error(); got != ErrConflict.Error()+": safe_unique_key" {
		t.Fatalf("constraint diagnostic = %q", got)
	}
}
