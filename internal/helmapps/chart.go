package helmapps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

type ChartBundle struct {
	PackageBytes      []byte
	ValuesSchemaJSON  []byte
	DefaultValuesYAML []byte
	ChartName         string
	ChartVersion      string
	PackageDigest     string
	SchemaDigest      string
	FileCount         int
	ExpandedBytes     int
}

var forbiddenTemplateFunctionRE = regexp.MustCompile(`(?m)(^|[^A-Za-z0-9_])(lookup|tpl|getHostByName|randAlpha|randAlphaNum|randAscii|randNumeric|randBytes|uuidv4|now|ago|date|dateInZone|htmlDate|unixEpoch|genCA|genPrivateKey|genSelfSignedCert|genSignedCert|encryptAES)([^A-Za-z0-9_]|$)`)

// InspectChartPackage verifies the exact approved blob and the subset of Helm
// chart structure that can be rendered offline. P0 rejects dependencies rather
// than allowing a renderer to resolve any dependency URL.
func InspectChartPackage(approval Approval, packageBytes []byte) (ChartBundle, error) {
	if approval.Validate() != nil || len(packageBytes) == 0 || len(packageBytes) > MaximumChartSize || digestBytes(packageBytes) != approval.PackageDigest {
		return ChartBundle{}, ErrUnsafeChart
	}
	chartName := approval.OCIRepository[strings.LastIndexByte(approval.OCIRepository, '/')+1:]
	gzipReader, err := gzip.NewReader(bytes.NewReader(packageBytes))
	if err != nil {
		return ChartBundle{}, ErrUnsafeChart
	}
	defer gzipReader.Close() //nolint:errcheck
	gzipReader.Multistream(false)
	tarReader := tar.NewReader(gzipReader)
	var chartYAML, schemaJSON, defaultValues []byte
	seenPaths := make(map[string]struct{})
	files, expanded := 0, 0
	for {
		header, nextErr := tarReader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil || header == nil || header.Name == "" || strings.Contains(header.Name, "\\") || path.IsAbs(header.Name) {
			return ChartBundle{}, ErrUnsafeChart
		}
		cleaned := path.Clean(header.Name)
		if cleaned != header.Name || cleaned == "." || strings.HasPrefix(cleaned, "../") ||
			(cleaned != chartName && !strings.HasPrefix(cleaned, chartName+"/")) {
			return ChartBundle{}, ErrUnsafeChart
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA || header.Size < 0 || header.Size > MaximumFileSize {
			return ChartBundle{}, ErrUnsafeChart
		}
		files++
		if _, duplicate := seenPaths[cleaned]; duplicate {
			return ChartBundle{}, ErrUnsafeChart
		}
		seenPaths[cleaned] = struct{}{}
		expanded += int(header.Size)
		if files > MaximumChartFiles || expanded > MaximumExpandSize {
			return ChartBundle{}, ErrUnsafeChart
		}
		relative := strings.TrimPrefix(cleaned, chartName+"/")
		if relative == "crds" || strings.HasPrefix(relative, "crds/") || relative == "charts" || strings.HasPrefix(relative, "charts/") {
			return ChartBundle{}, ErrUnsafeChart
		}
		content, readErr := io.ReadAll(io.LimitReader(tarReader, MaximumFileSize+1))
		if readErr != nil || len(content) != int(header.Size) || len(content) > MaximumFileSize || bytes.IndexByte(content, 0) >= 0 {
			return ChartBundle{}, ErrUnsafeChart
		}
		switch relative {
		case "Chart.yaml":
			if chartYAML != nil {
				return ChartBundle{}, ErrUnsafeChart
			}
			chartYAML = content
		case "values.schema.json":
			if schemaJSON != nil || len(content) == 0 || len(content) > MaximumSchemaSize {
				return ChartBundle{}, ErrUnsafeChart
			}
			schemaJSON = content
		case "values.yaml":
			if defaultValues != nil {
				return ChartBundle{}, ErrUnsafeChart
			}
			defaultValues = content
		default:
			if strings.HasPrefix(relative, "templates/") && forbiddenTemplateFunctionRE.Match(content) {
				return ChartBundle{}, ErrUnsafeChart
			}
		}
	}
	if files == 0 || chartYAML == nil || schemaJSON == nil || defaultValues == nil || digestBytes(schemaJSON) != approval.ValuesSchemaDigest {
		return ChartBundle{}, ErrUnsafeChart
	}
	if err = validateChartMetadata(chartYAML, chartName, approval.ChartVersion); err != nil {
		return ChartBundle{}, err
	}
	if _, err = compileValuesSchema(schemaJSON); err != nil {
		return ChartBundle{}, err
	}
	if _, parseErr := ParseValues(defaultValues); parseErr != nil {
		return ChartBundle{}, ErrUnsafeChart
	}
	return ChartBundle{PackageBytes: append([]byte(nil), packageBytes...), ValuesSchemaJSON: append([]byte(nil), schemaJSON...),
		DefaultValuesYAML: append([]byte(nil), defaultValues...),
		ChartName:         chartName, ChartVersion: approval.ChartVersion, PackageDigest: approval.PackageDigest,
		SchemaDigest: approval.ValuesSchemaDigest, FileCount: files, ExpandedBytes: expanded}, nil
}

func validateChartMetadata(raw []byte, expectedName, expectedVersion string) error {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return ErrUnsafeChart
	}
	node, err := decodeSingleYAML(raw, true)
	if err != nil || node.Kind != yaml.MappingNode || validateYAMLTree(node, 2048, 16) != nil {
		return ErrUnsafeChart
	}
	fields := make(map[string]*yaml.Node, len(node.Content)/2)
	for index := 0; index < len(node.Content); index += 2 {
		fields[node.Content[index].Value] = node.Content[index+1]
	}
	if _, dependencies := fields["dependencies"]; dependencies {
		return ErrUnsafeChart
	}
	apiVersion, apiOK := scalarString(fields["apiVersion"])
	name, nameOK := scalarString(fields["name"])
	version, versionOK := scalarString(fields["version"])
	if !apiOK || !nameOK || !versionOK || apiVersion != "v2" || name != expectedName || version != expectedVersion {
		return ErrUnsafeChart
	}
	if chartType, present := fields["type"]; present {
		value, ok := scalarString(chartType)
		if !ok || value != "application" {
			return ErrUnsafeChart
		}
	}
	return nil
}

func scalarString(node *yaml.Node) (string, bool) {
	if node == nil || node.Kind != yaml.ScalarNode || node.Tag != "!!str" || node.Value == "" {
		return "", false
	}
	return node.Value, true
}

func compileValuesSchema(raw []byte) (*jsonschema.Schema, error) {
	if len(raw) == 0 || len(raw) > MaximumSchemaSize {
		return nil, ErrUnsafeChart
	}
	document, err := decodeStrictJSON(raw)
	if err != nil || rejectRemoteSchemaReferences(document) != nil || validateClosedValuesSchema(document) != nil {
		return nil, ErrUnsafeChart
	}
	compiler := jsonschema.NewCompiler()
	if err = compiler.AddResource("values.schema.json", document); err != nil {
		return nil, ErrUnsafeChart
	}
	compiled, err := compiler.Compile("values.schema.json")
	if err != nil {
		return nil, ErrUnsafeChart
	}
	return compiled, nil
}

func validateClosedValuesSchema(document any) error {
	root, ok := document.(map[string]any)
	if !ok || root["type"] != "object" {
		return ErrUnsafeChart
	}
	var walk func(any) error
	walk = func(value any) error {
		switch typed := value.(type) {
		case map[string]any:
			_, hasProperties := typed["properties"]
			if typed["type"] == "object" || hasProperties {
				closed, ok := typed["additionalProperties"].(bool)
				if !ok || closed {
					return ErrUnsafeChart
				}
				if _, present := typed["patternProperties"]; present {
					return ErrUnsafeChart
				}
			}
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		case []any:
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(document)
}

func ValidateValuesSchema(schemaJSON []byte, values ParsedValues) error {
	compiled, err := compileValuesSchema(schemaJSON)
	if err != nil || values.Values == nil {
		return ErrUnsafeChart
	}
	if err = compiled.Validate(values.Values); err != nil {
		return fmt.Errorf("%w: values.schema.json rejected values.yaml", ErrUnsafeYAML)
	}
	return nil
}

func validateMergedValuesSchema(schemaJSON []byte, defaults, overrides ParsedValues) error {
	if defaults.Values == nil || overrides.Values == nil {
		return ErrUnsafeYAML
	}
	merged := cloneJSONMap(defaults.Values)
	mergeJSONMaps(merged, overrides.Values)
	return ValidateValuesSchema(schemaJSON, ParsedValues{Values: merged, Raw: overrides.Raw})
}

func cloneJSONMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		if child, ok := value.(map[string]any); ok {
			result[key] = cloneJSONMap(child)
		} else {
			result[key] = value
		}
	}
	return result
}

func mergeJSONMaps(destination, overrides map[string]any) {
	for key, value := range overrides {
		childOverride, overrideIsMap := value.(map[string]any)
		childDestination, destinationIsMap := destination[key].(map[string]any)
		if overrideIsMap && destinationIsMap {
			mergeJSONMaps(childDestination, childOverride)
			continue
		}
		destination[key] = value
	}
}

func decodeStrictJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	nodes := 0
	var decode func(int) (any, error)
	decode = func(depth int) (any, error) {
		if depth > MaximumYAMLDepth {
			return nil, ErrUnsafeChart
		}
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		nodes++
		if nodes > MaximumYAMLNodes {
			return nil, ErrUnsafeChart
		}
		switch typed := token.(type) {
		case json.Delim:
			switch typed {
			case '{':
				object := make(map[string]any)
				for decoder.More() {
					keyToken, keyErr := decoder.Token()
					key, ok := keyToken.(string)
					if keyErr != nil || !ok || key == "" || len(key) > 256 {
						return nil, ErrUnsafeChart
					}
					if _, duplicate := object[key]; duplicate {
						return nil, ErrUnsafeChart
					}
					value, valueErr := decode(depth + 1)
					if valueErr != nil {
						return nil, valueErr
					}
					object[key] = value
				}
				if closeToken, closeErr := decoder.Token(); closeErr != nil || closeToken != json.Delim('}') {
					return nil, ErrUnsafeChart
				}
				return object, nil
			case '[':
				array := make([]any, 0)
				for decoder.More() {
					value, valueErr := decode(depth + 1)
					if valueErr != nil {
						return nil, valueErr
					}
					array = append(array, value)
				}
				if closeToken, closeErr := decoder.Token(); closeErr != nil || closeToken != json.Delim(']') {
					return nil, ErrUnsafeChart
				}
				return array, nil
			}
			return nil, ErrUnsafeChart
		case string:
			if len(typed) > 64<<10 {
				return nil, ErrUnsafeChart
			}
			return typed, nil
		case json.Number, bool, nil:
			return typed, nil
		default:
			return nil, ErrUnsafeChart
		}
	}
	value, err := decode(1)
	if err != nil {
		return nil, err
	}
	if token, trailingErr := decoder.Token(); !errors.Is(trailingErr, io.EOF) || token != nil {
		return nil, ErrUnsafeChart
	}
	return value, nil
}

func rejectRemoteSchemaReferences(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "$id" || key == "$schema" || key == "$vocabulary" {
				if key == "$id" {
					return ErrUnsafeChart
				}
			}
			if key == "$ref" || key == "$dynamicRef" {
				reference, ok := child.(string)
				if !ok || !strings.HasPrefix(reference, "#") {
					return ErrUnsafeChart
				}
			}
			if err := rejectRemoteSchemaReferences(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := rejectRemoteSchemaReferences(child); err != nil {
				return err
			}
		}
	}
	return nil
}
