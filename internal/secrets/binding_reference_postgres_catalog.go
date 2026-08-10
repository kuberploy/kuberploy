package secrets

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// PostgreSQLBindingReferenceCatalogTx exposes the safe metadata resolver on an
// existing transaction. It locks the selected binding and its versions so a
// caller can resolve an AppConfig and reconcile its deletion guards without a
// second connection or cross-transaction time-of-check/time-of-use window.
type PostgreSQLBindingReferenceCatalogTx struct{ tx pgx.Tx }

func NewPostgreSQLBindingReferenceCatalogTx(tx pgx.Tx) (*PostgreSQLBindingReferenceCatalogTx, error) {
	if tx == nil {
		return nil, ErrInvalid
	}
	return &PostgreSQLBindingReferenceCatalogTx{tx: tx}, nil
}

func (c *PostgreSQLBindingReferenceCatalogTx) Binding(ctx context.Context, bindingID string) (Binding, error) {
	if c == nil || c.tx == nil || !uuidRE.MatchString(bindingID) {
		return Binding{}, ErrInvalid
	}
	return readBinding(ctx, c.tx, bindingID, true)
}

func (c *PostgreSQLBindingReferenceCatalogTx) Versions(ctx context.Context, bindingID string) ([]Version, error) {
	if c == nil || c.tx == nil || !uuidRE.MatchString(bindingID) {
		return nil, ErrInvalid
	}
	return versionsInTx(ctx, c.tx, bindingID)
}

var _ BindingReferenceCatalog = (*PostgreSQLBindingReferenceCatalogTx)(nil)
