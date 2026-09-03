package mcpserver

import (
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
)

type numericDomain struct {
	minimum    float64
	maximum    float64
	hasMaximum bool
}

type arrayDomain struct {
	maxItems      int
	itemMaxLength int
}

type inputDomains struct {
	numeric map[string]numericDomain
	strings map[string]int
	arrays  map[string]arrayDomain
}

// numericInputSchema adds the domains Go's integer type cannot express. The SDK validates the
// resolved schema before decoding or calling a handler, so an agent typo fails at the MCP
// boundary exactly as the corresponding CLI flag does.
func boundedInputSchema[T any](domains inputDomains) (*jsonschema.Schema, error) {
	schema, err := jsonschema.For[T](nil)
	if err != nil {
		return nil, fmt.Errorf("infer MCP input schema: %w", err)
	}
	for name, domain := range domains.numeric {
		property, exists := schema.Properties[name]
		if !exists {
			return nil, fmt.Errorf("infer MCP input schema: numeric property %q is absent", name)
		}
		property.Minimum = jsonschema.Ptr(domain.minimum)
		if domain.hasMaximum {
			property.Maximum = jsonschema.Ptr(domain.maximum)
		}
	}
	for name, maxLength := range domains.strings {
		property, exists := schema.Properties[name]
		if !exists {
			return nil, fmt.Errorf("infer MCP input schema: string property %q is absent", name)
		}
		property.MaxLength = jsonschema.Ptr(maxLength)
	}
	for name, domain := range domains.arrays {
		property, exists := schema.Properties[name]
		if !exists || property.Items == nil {
			return nil, fmt.Errorf("infer MCP input schema: array property %q is absent", name)
		}
		property.MaxItems = jsonschema.Ptr(domain.maxItems)
		property.Items.MaxLength = jsonschema.Ptr(domain.itemMaxLength)
	}
	return schema, nil
}
