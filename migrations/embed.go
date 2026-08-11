package migrations

import "embed"

// FS contains the immutable database migrations shipped with the API binary.
//
//go:embed *.sql
var FS embed.FS

// CurrentSchema is bumped with every ordered migration and is compared with
// the verified release manifest before accepting a platform upgrade.
const CurrentSchema = "046_git_projection_chart_versions"
