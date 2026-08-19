package specadapter

import (
	"fmt"
	"sort"

	"github.com/alexandremahdhaoui/forge-factory/internal/adapter/fsadapter"
	"github.com/alexandremahdhaoui/forge-factory/internal/types/docstypes"
	"sigs.k8s.io/yaml"
)

type document struct {
	Components struct {
		Schemas map[string]schema `json:"schemas"`
	} `json:"components"`
}

type schema struct {
	Type        string            `json:"type"`
	Description string            `json:"description"`
	Required    []string          `json:"required"`
	Properties  map[string]schema `json:"properties"`
	Items       *schema           `json:"items"`
	Ref         string            `json:"$ref"`

	AdditionalProperties *schema `json:"additionalProperties"`
}

// Read parses an OpenAPI document into the shape the docs need. It reads the
// same file the engines generate their types from, so a doc cannot describe a
// field the wire does not carry.
func Read(fs fsadapter.FS, path string) (map[string]docstypes.Schema, error) {
	raw, err := fs.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var doc document

	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("reading the spec %s: %w", path, err)
	}

	if len(doc.Components.Schemas) == 0 {
		return nil, fmt.Errorf("reading the spec %s: it declares no schemas", path)
	}

	out := make(map[string]docstypes.Schema, len(doc.Components.Schemas))

	for name, s := range doc.Components.Schemas {
		out[name] = convert(name, s)
	}

	return out, nil
}

func convert(name string, s schema) docstypes.Schema {
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}

	fields := make([]string, 0, len(s.Properties))
	for field := range s.Properties {
		fields = append(fields, field)
	}

	sort.Strings(fields)

	props := make([]docstypes.Property, 0, len(fields))

	for _, field := range fields {
		p := s.Properties[field]

		props = append(props, docstypes.Property{
			Name:        field,
			Type:        typeOf(p),
			Required:    required[field],
			Description: p.Description,
		})
	}

	return docstypes.Schema{Name: name, Description: s.Description, Properties: props}
}

func typeOf(s schema) string {
	if s.Ref != "" {
		return refName(s.Ref)
	}

	switch s.Type {
	case "array":
		if s.Items == nil {
			return "array"
		}

		return "array of " + typeOf(*s.Items)
	case "object":
		if s.AdditionalProperties != nil {
			return "map of " + typeOf(*s.AdditionalProperties)
		}

		return "object"
	case "":
		return "any"
	default:
		return s.Type
	}
}

func refName(ref string) string {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == '/' {
			return ref[i+1:]
		}
	}

	return ref
}
