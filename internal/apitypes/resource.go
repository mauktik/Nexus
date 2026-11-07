// Package apitypes holds the v1 wire types. Field names and shapes are part of
// the versioned contract (FR-30, NFR-5): change them only additively.
package apitypes

// Resource is one managed entity (FR-1). P is the provider-specific
// properties type; clients that do not know it use json.RawMessage.
type Resource[P any] struct {
	ID         string `json:"id"`   // /providers/{provider}/{type}/{name}
	Type       string `json:"type"` // Resource type, e.g. "containers"
	Name       string `json:"name"`
	Host       string `json:"host"`
	Properties P      `json:"properties"`
}

// ResourceID builds the stable resource ID /providers/{provider}/{type}/{name}.
func ResourceID(provider, resourceType, name string) string {
	return "/providers/" + provider + "/" + resourceType + "/" + name
}

// NewResource builds a Resource with its ID derived from provider, type and name.
func NewResource[P any](host, provider, resourceType, name string, properties P) Resource[P] {
	return Resource[P]{
		ID:         ResourceID(provider, resourceType, name),
		Type:       resourceType,
		Name:       name,
		Host:       host,
		Properties: properties,
	}
}
