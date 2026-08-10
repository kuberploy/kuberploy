// Package schema embeds Kuberploy desired-state schemas.
package schema

import _ "embed"

//go:embed appconfig-v1alpha1.schema.json
var AppConfig []byte
