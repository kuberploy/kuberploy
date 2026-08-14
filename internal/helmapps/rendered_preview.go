package helmapps

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.yaml.in/yaml/v3"
)

const (
	MaximumSanitizedResourcePreviewBytes = 32 << 10
	MaximumSanitizedManifestPreviewBytes = 256 << 10
	redactedPreviewValue                 = "[REDACTED]"
)

// RenderedResourcePreview preserves the verified inventory identity and adds
// a server-produced, bounded YAML projection. SanitizedYAML is never the raw
// renderer output. Oversized resources remain in inventory and are marked
// omitted instead of weakening the response-wide byte limit.
type RenderedResourcePreview struct {
	APIVersion     string `json:"apiVersion"`
	Kind           string `json:"kind"`
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	SanitizedYAML  string `json:"sanitizedYaml,omitempty"`
	PreviewOmitted bool   `json:"previewOmitted"`
}

type RenderedManifestPreview struct {
	ReleaseRevisionID string                    `json:"releaseRevisionId"`
	Generation        int64                     `json:"generation"`
	Target            ReleaseTarget             `json:"target"`
	ManifestDigest    string                    `json:"manifestDigest"`
	InventoryDigest   string                    `json:"inventoryDigest"`
	ResourceCount     int                       `json:"resourceCount"`
	PreviewBytes      int                       `json:"previewBytes"`
	Resources         []RenderedResourcePreview `json:"resources"`
}

type PostgresRenderedManifestPreviewService struct{ pool *pgxpool.Pool }

func NewPostgresRenderedManifestPreviewService(pool *pgxpool.Pool) (*PostgresRenderedManifestPreviewService, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &PostgresRenderedManifestPreviewService{pool: pool}, nil
}

// Preview resolves only the current exact environment/application release
// head and requires that head's own render command and result to be succeeded
// and internally self-consistent before returning a bounded inventory.
func (s *PostgresRenderedManifestPreviewService) Preview(ctx context.Context,
	target ReleaseTarget) (RenderedManifestPreview, error) {
	if ctx == nil || target.Validate() != nil {
		return RenderedManifestPreview{}, ErrInvalid
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		return RenderedManifestPreview{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var revisionID, commandID string
	var generation int64
	var desiredEnabled bool
	err = tx.QueryRow(ctx, `SELECT release.id::text,release.generation,
		release.desired_enabled,COALESCE(release.render_command_id::text,'')
		FROM helm_release_heads head
		JOIN helm_release_revisions release ON release.id=head.revision_id
		WHERE head.environment_id=$1 AND head.application_id=$2 AND
			release.project_id=$3 AND release.environment_id=$1 AND release.application_id=$2`,
		target.EnvironmentID, target.ApplicationID,
		target.ProjectID).Scan(&revisionID, &generation, &desiredEnabled, &commandID)
	if errors.Is(err, pgx.ErrNoRows) {
		return RenderedManifestPreview{}, ErrNotFound
	}
	if err != nil {
		return RenderedManifestPreview{}, classifyPostgres(err)
	}
	if !desiredEnabled || !uuidRE.MatchString(commandID) || generation < 1 {
		return RenderedManifestPreview{}, ErrConflict
	}
	command, err := scanCommand(tx.QueryRow(ctx, commandSelect+`
		WHERE c.id=$1 AND c.project_id=$2 AND c.environment_id=$3 AND c.application_id=$4`,
		commandID, target.ProjectID, target.EnvironmentID, target.ApplicationID))
	if err != nil {
		return RenderedManifestPreview{}, classifyPostgres(err)
	}
	result, err := scanResult(tx.QueryRow(ctx, resultSelect+` WHERE command_id=$1`, commandID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RenderedManifestPreview{}, ErrNotFound
	}
	if err != nil {
		return RenderedManifestPreview{}, classifyPostgres(err)
	}
	validated, validationErr := ValidateRenderedManifests(result.RenderedManifests, command.Descriptor)
	if command.State != StateSucceeded || result.Validate(command) != nil || validationErr != nil ||
		result.ManifestDigest != validated.ManifestDigest || result.InventoryDigest != validated.InventoryDigest ||
		result.ResourceCount != validated.ResourceCount {
		return RenderedManifestPreview{}, ErrConflict
	}
	resources, previewBytes, err := renderedResourceInventory(validated.Raw, command.Descriptor.Destination.Namespace)
	if err != nil || len(resources) != validated.ResourceCount {
		return RenderedManifestPreview{}, ErrConflict
	}
	if err = tx.Commit(ctx); err != nil {
		return RenderedManifestPreview{}, classifyPostgres(err)
	}
	return RenderedManifestPreview{ReleaseRevisionID: revisionID, Generation: generation,
		Target: target, ManifestDigest: result.ManifestDigest, InventoryDigest: result.InventoryDigest,
		ResourceCount: result.ResourceCount, PreviewBytes: previewBytes, Resources: resources}, nil
}

func renderedResourceInventory(raw []byte, defaultNamespace string) ([]RenderedResourcePreview, int, error) {
	if len(raw) == 0 || len(raw) > MaximumOutputSize || !dnsLabelRE.MatchString(defaultNamespace) {
		return nil, 0, ErrInvalid
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	resources := make([]RenderedResourcePreview, 0)
	for {
		var document yaml.Node
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || len(document.Content) != 1 || document.Content[0] == nil ||
			document.Content[0].Kind != yaml.MappingNode ||
			validateYAMLTree(document.Content[0], MaximumYAMLNodes, MaximumYAMLDepth) != nil {
			return nil, 0, ErrUnsafeChart
		}
		var resource map[string]any
		if err = document.Content[0].Decode(&resource); err != nil {
			return nil, 0, ErrUnsafeChart
		}
		apiVersion, apiOK := resource["apiVersion"].(string)
		kind, kindOK := resource["kind"].(string)
		metadata, metadataOK := resource["metadata"].(map[string]any)
		if !apiOK || !kindOK || !metadataOK {
			return nil, 0, ErrUnsafeChart
		}
		name, nameOK := metadata["name"].(string)
		namespace := defaultNamespace
		if explicit, exists := metadata["namespace"]; exists {
			namespace, _ = explicit.(string)
		}
		if !nameOK || apiVersion == "" || kind == "" || !dnsSubdomainRE.MatchString(name) ||
			!dnsLabelRE.MatchString(namespace) {
			return nil, 0, ErrUnsafeChart
		}
		sanitized, sanitizeErr := sanitizedResourceYAML(document.Content[0], kind)
		if sanitizeErr != nil {
			return nil, 0, ErrUnsafeChart
		}
		resources = append(resources, RenderedResourcePreview{APIVersion: apiVersion,
			Kind: kind, Namespace: namespace, Name: name, SanitizedYAML: sanitized})
		if len(resources) > MaximumResources {
			return nil, 0, ErrUnsafeChart
		}
	}
	if len(resources) == 0 {
		return nil, 0, ErrUnsafeChart
	}
	sort.Slice(resources, func(i, j int) bool {
		left, right := resources[i], resources[j]
		if left.APIVersion != right.APIVersion {
			return left.APIVersion < right.APIVersion
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.Namespace != right.Namespace {
			return left.Namespace < right.Namespace
		}
		return left.Name < right.Name
	})
	previewBytes := 0
	for index := range resources {
		length := len(resources[index].SanitizedYAML)
		if length > MaximumSanitizedResourcePreviewBytes ||
			previewBytes+length > MaximumSanitizedManifestPreviewBytes {
			resources[index].SanitizedYAML = ""
			resources[index].PreviewOmitted = true
			continue
		}
		previewBytes += length
	}
	return resources, previewBytes, nil
}

// sanitizedResourceYAML retains only declarative identity/spec fields. It
// removes status and unknown top-level renderer metadata, canonicalizes map
// order, and redacts Kubernetes secret-bearing containers plus sensitive
// leaves before any bytes become caller-visible.
func sanitizedResourceYAML(source *yaml.Node, kind string) (string, error) {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for index := 0; index+1 < len(source.Content); index += 2 {
		key, value := source.Content[index], source.Content[index+1]
		if key.Kind != yaml.ScalarNode {
			return "", ErrUnsafeChart
		}
		switch key.Value {
		case "apiVersion", "kind", "metadata", "spec":
			projection := cloneYAMLNode(value)
			if key.Value == "metadata" {
				projection = sanitizedMetadataYAML(value)
			}
			root.Content = append(root.Content, cloneYAMLNode(key), projection)
		case "data", "binaryData", "stringData":
			// ConfigMaps and Secrets can carry arbitrary credentials. Never retain
			// either their keys or values in a preview.
			if kind == "ConfigMap" || kind == "Secret" {
				root.Content = append(root.Content, cloneYAMLNode(key), redactedYAMLNode())
			}
		}
	}
	sanitizeYAMLNode(root, nil)
	canonicalizeYAMLMaps(root)
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(root); err != nil {
		return "", err
	}
	if err := encoder.Close(); err != nil {
		return "", err
	}
	return output.String(), nil
}

func cloneYAMLNode(source *yaml.Node) *yaml.Node {
	clone := *source
	clone.HeadComment = ""
	clone.LineComment = ""
	clone.FootComment = ""
	clone.Anchor = ""
	clone.Alias = nil
	clone.Content = make([]*yaml.Node, len(source.Content))
	for index, child := range source.Content {
		clone.Content[index] = cloneYAMLNode(child)
	}
	return &clone
}

func sanitizedMetadataYAML(source *yaml.Node) *yaml.Node {
	projection := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if source.Kind != yaml.MappingNode {
		return projection
	}
	for index := 0; index+1 < len(source.Content); index += 2 {
		key, value := source.Content[index], source.Content[index+1]
		switch key.Value {
		case "name", "namespace":
			projection.Content = append(projection.Content, cloneYAMLNode(key), cloneYAMLNode(value))
		case "annotations":
			projection.Content = append(projection.Content, cloneYAMLNode(key), redactedYAMLNode())
		}
	}
	return projection
}

func redactedYAMLNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: redactedPreviewValue}
}

func sanitizeYAMLNode(node *yaml.Node, path []string) {
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			name := strings.ToLower(strings.TrimSpace(key.Value))
			if name == "command" || name == "args" || name == "annotations" || sensitivePreviewLeaf(name) ||
				(name == "value" && pathContains(path, "env")) {
				node.Content[index+1] = redactedYAMLNode()
				continue
			}
			sanitizeYAMLNode(value, append(path, name))
		}
	case yaml.SequenceNode:
		for _, child := range node.Content {
			sanitizeYAMLNode(child, path)
		}
	}
}

func sensitivePreviewLeaf(name string) bool {
	if strings.HasSuffix(name, "secretref") || strings.HasSuffix(name, "secretkeyref") ||
		strings.HasSuffix(name, "secretname") {
		return false
	}
	for _, token := range []string{"password", "passwd", "token", "credential", "privatekey",
		"private-key", "apikey", "api-key", "clientsecret", "client-secret", "authorization",
		"bearer", "cookie", "sessionkey", "session-key", "secret", "accesskey", "access-key",
		"signingkey", "signing-key", "encryptionkey", "encryption-key", "connectionstring",
		"connection-string", "databaseurl", "database-url", "dsn", "dockerconfigjson", "sshkey",
		"ssh-key"} {
		if strings.Contains(name, token) {
			return true
		}
	}
	return false
}

func pathContains(path []string, value string) bool {
	for _, item := range path {
		if item == value {
			return true
		}
	}
	return false
}

func canonicalizeYAMLMaps(node *yaml.Node) {
	for _, child := range node.Content {
		canonicalizeYAMLMaps(child)
	}
	if node.Kind != yaml.MappingNode {
		return
	}
	type pair struct{ key, value *yaml.Node }
	pairs := make([]pair, 0, len(node.Content)/2)
	for index := 0; index+1 < len(node.Content); index += 2 {
		pairs = append(pairs, pair{node.Content[index], node.Content[index+1]})
	}
	sort.SliceStable(pairs, func(i, j int) bool { return pairs[i].key.Value < pairs[j].key.Value })
	node.Content = node.Content[:0]
	for _, item := range pairs {
		node.Content = append(node.Content, item.key, item.value)
	}
}
