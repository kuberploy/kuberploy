package helmapps

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"
)

type ParsedValues struct {
	Raw    []byte
	Values map[string]any
}

// ParseValues accepts exactly one bounded JSON-compatible YAML mapping. YAML
// graph features are rejected so the stored bytes have one unambiguous tree.
func ParseValues(raw []byte) (ParsedValues, error) {
	normalized, err := normalizeDocument(raw, MaximumValuesSize)
	if err != nil {
		return ParsedValues{}, ErrUnsafeYAML
	}
	node, err := decodeSingleYAML(normalized, true)
	if err != nil || node.Kind != yaml.MappingNode {
		return ParsedValues{}, ErrUnsafeYAML
	}
	if err = validateYAMLTree(node, MaximumYAMLNodes, MaximumYAMLDepth); err != nil {
		return ParsedValues{}, err
	}
	var decoded any
	if err = node.Decode(&decoded); err != nil {
		return ParsedValues{}, ErrUnsafeYAML
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		return ParsedValues{}, ErrUnsafeYAML
	}
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err = decoder.Decode(&values); err != nil || values == nil {
		return ParsedValues{}, ErrUnsafeYAML
	}
	if err = rejectCallerControlValues(values, ""); err != nil {
		return ParsedValues{}, err
	}
	return ParsedValues{Raw: normalized, Values: values}, nil
}

func normalizeDocument(raw []byte, maximum int) ([]byte, error) {
	if len(raw) == 0 || len(raw) > maximum || bytes.IndexByte(raw, 0) >= 0 || bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		return nil, ErrUnsafeYAML
	}
	normalized := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	normalized = bytes.ReplaceAll(normalized, []byte("\r"), []byte("\n"))
	if !bytes.HasSuffix(normalized, []byte("\n")) {
		normalized = append(append([]byte(nil), normalized...), '\n')
	} else {
		normalized = append([]byte(nil), normalized...)
	}
	return normalized, nil
}

func decodeSingleYAML(raw []byte, requireEOF bool) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil || len(document.Content) != 1 || document.Content[0] == nil {
		return nil, ErrUnsafeYAML
	}
	if requireEOF {
		var extra yaml.Node
		err := decoder.Decode(&extra)
		if err == nil || !errors.Is(err, io.EOF) {
			return nil, ErrUnsafeYAML
		}
	}
	return document.Content[0], nil
}

func validateYAMLTree(root *yaml.Node, maximumNodes, maximumDepth int) error {
	nodes := 0
	var walk func(*yaml.Node, int) error
	walk = func(node *yaml.Node, depth int) error {
		if node == nil || depth > maximumDepth {
			return ErrUnsafeYAML
		}
		nodes++
		if nodes > maximumNodes || node.Anchor != "" || node.Alias != nil || node.Kind == yaml.AliasNode || node.Style&yaml.TaggedStyle != 0 {
			return ErrUnsafeYAML
		}
		switch node.Kind {
		case yaml.MappingNode:
			if len(node.Content)%2 != 0 {
				return ErrUnsafeYAML
			}
			seen := make(map[string]struct{}, len(node.Content)/2)
			for index := 0; index < len(node.Content); index += 2 {
				key := node.Content[index]
				if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Value == "" || key.Value == "<<" || len(key.Value) > 256 {
					return ErrUnsafeYAML
				}
				if _, exists := seen[key.Value]; exists {
					return ErrUnsafeYAML
				}
				seen[key.Value] = struct{}{}
			}
		case yaml.SequenceNode:
		case yaml.ScalarNode:
			if len(node.Value) > 64<<10 {
				return ErrUnsafeYAML
			}
			switch node.Tag {
			case "!!str", "!!bool", "!!int", "!!float", "!!null":
			default:
				return ErrUnsafeYAML
			}
		default:
			return ErrUnsafeYAML
		}
		for _, child := range node.Content {
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(root, 1)
}

var forbiddenLeafKeys = map[string]struct{}{
	"password": {}, "passwd": {}, "token": {}, "accesstoken": {}, "refreshtoken": {},
	"apikey": {}, "clientsecret": {}, "privatekey": {}, "credential": {}, "credentials": {},
	"accesskey": {}, "secretkey": {}, "sshkey": {}, "connectionstring": {}, "dsn": {},
	"username": {}, "clientid": {}, "secret": {}, "dockerconfigjson": {}, "stringdata": {},
	"secretdata": {}, "authorization": {}, "auth": {},
	"namespace": {}, "targetnamespace": {}, "releasenamespace": {}, "kubecontext": {},
	"postrenderer": {}, "postrenderers": {}, "skipschemavalidation": {}, "passcredentials": {},
	"dependencyupdate": {}, "includecrds": {}, "skipcrds": {},
}

var secretReferenceKeys = map[string]struct{}{
	"existingsecret": {}, "existingsecretname": {}, "secretname": {}, "secretref": {},
}

func rejectCallerControlValues(value any, path string) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := normalizeKey(key)
			childPath := path + "/" + key
			if _, reference := secretReferenceKeys[normalized]; reference {
				name, ok := child.(string)
				if !ok || !dnsLabelRE.MatchString(name) {
					return fmt.Errorf("%w: %s must be one Secret name reference", ErrUnsafeYAML, childPath)
				}
				continue
			}
			if _, forbidden := forbiddenLeafKeys[normalized]; forbidden {
				return fmt.Errorf("%w: %s is caller-controlled credential or renderer metadata", ErrUnsafeYAML, childPath)
			}
			if err := rejectCallerControlValues(child, childPath); err != nil {
				return err
			}
		}
	case []any:
		for index, child := range typed {
			if err := rejectCallerControlValues(child, fmt.Sprintf("%s/%d", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func normalizeKey(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, value)
}
