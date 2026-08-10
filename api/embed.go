// Package api embeds the exact public contract served by the API binary.
package api

import _ "embed"

// OpenAPIYAML is JSON-form YAML: the same immutable bytes are valid as both
// application/json and application/yaml, preventing format drift.
//
//go:embed openapi.yaml
var OpenAPIYAML []byte

var OpenAPIJSON = OpenAPIYAML

// ArazzoYAML is a JSON-form YAML Arazzo 1.1 workflow description. Keeping the
// embedded bytes valid JSON as well as YAML makes contract validation
// deterministic and avoids a second YAML parser in the API binary.
//
//go:embed arazzo.yaml
var ArazzoYAML []byte
