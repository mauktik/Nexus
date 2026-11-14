package apitypes

import "encoding/json"

// CapabilitiesID is the fixed id of the capabilities object (FR-30).
const CapabilitiesID = "/capabilities"

// APIVersion is the major API version served under /v1.
const APIVersion = "v1"

// ProviderState is a compiled-in provider's availability (FR-2).
type ProviderState string

const (
	ProviderEnabled     ProviderState = "enabled"
	ProviderDisabled    ProviderState = "disabled"    // By config
	ProviderUnavailable ProviderState = "unavailable" // By failed preflight
)

// Capabilities is the response of GET /v1/capabilities (FR-2, NFR-5).
type Capabilities struct {
	ID         string               `json:"id"` // Always CapabilitiesID
	Host       string               `json:"host"`
	APIVersion string               `json:"apiVersion"`
	Version    string               `json:"version"` // nexusd build version
	OS         HostOS               `json:"os"`
	InitSystem string               `json:"initSystem"` // systemd, openrc or unknown
	Providers  []ProviderCapability `json:"providers"`
}

// HostOS identifies the host's operating system, as platform.OS reports it (FR-18).
// Every field is empty when the host has no OS identification file.
type HostOS struct {
	ID         string `json:"id"`
	VersionID  string `json:"versionId"`
	PrettyName string `json:"prettyName,omitempty"`
}

// ProviderCapability describes one compiled-in provider.
type ProviderCapability struct {
	Name          string         `json:"name"`
	Status        ProviderState  `json:"status"`
	Reason        string         `json:"reason,omitempty"` // Human-readable; set unless enabled
	Details       map[string]any `json:"details,omitempty"`
	ResourceTypes []string       `json:"resourceTypes"`
	Actions       []string       `json:"actions"`
}

// NewCapabilities builds a Capabilities with its fixed id and API version.
func NewCapabilities(host, version string, hostOS HostOS, initSystem string, providers []ProviderCapability) Capabilities {
	return Capabilities{
		ID:         CapabilitiesID,
		Host:       host,
		APIVersion: APIVersion,
		Version:    version,
		OS:         hostOS,
		InitSystem: initSystem,
		Providers:  providers,
	}
}

type capabilitiesWire Capabilities

// MarshalJSON writes nil lists as [], never null.
func (c Capabilities) MarshalJSON() ([]byte, error) {
	w := capabilitiesWire(c)
	if w.Providers == nil {
		w.Providers = []ProviderCapability{}
	}
	return json.Marshal(w)
}

type providerCapabilityWire ProviderCapability

// MarshalJSON writes nil lists as [], never null.
func (p ProviderCapability) MarshalJSON() ([]byte, error) {
	w := providerCapabilityWire(p)
	if w.ResourceTypes == nil {
		w.ResourceTypes = []string{}
	}
	if w.Actions == nil {
		w.Actions = []string{}
	}
	return json.Marshal(w)
}
