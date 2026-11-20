package apitypes

import (
	"encoding/json"
	"time"
)

// Severity words of a HealthSignal. Clients must accept words they do not know.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
	SeverityInfo     = "info"
)

// HealthSignal is one finding of a provider's health read.
type HealthSignal struct {
	ID         string    `json:"id" jsonschema:"A stable code, e.g. pool-not-online."`
	Severity   string    `json:"severity" jsonschema:"critical, warning or info."`
	Subject    string    `json:"subject" jsonschema:"What the signal is about, e.g. pool:z1; host-originated."`
	Message    string    `json:"message" jsonschema:"A fixed template filled with host-originated names."`
	ObservedAt time.Time `json:"observedAt"`
}

// SignalCounts counts the signals of a health read by severity.
type SignalCounts struct {
	Critical int `json:"critical"`
	Warning  int `json:"warning"`
	Info     int `json:"info"`
}

// ProviderSignals is one provider's block in host.health.get's providerSignals.
type ProviderSignals struct {
	Source           string            `json:"source" jsonschema:"The provider name."`
	ObservedAt       *time.Time        `json:"observedAt"`
	Counts           *SignalCounts     `json:"counts" jsonschema:"Null when the source could not be read; unavailable says why."`
	Signals          []HealthSignal    `json:"signals" jsonschema:"At most 10, critical first."`
	SignalsTruncated bool              `json:"signalsTruncated"`
	Stale            bool              `json:"stale" jsonschema:"True: this is the source's last completed report, because the current read failed or missed the deadline."`
	Unavailable      map[string]string `json:"unavailable,omitempty" jsonschema:"signals: why there are none, or why they are stale; else per-feature reasons."`
}

// providerSignalsWire is ProviderSignals without its MarshalJSON.
type providerSignalsWire ProviderSignals

// MarshalJSON writes a nil Signals as [], never null.
func (p ProviderSignals) MarshalJSON() ([]byte, error) {
	w := providerSignalsWire(p)
	if w.Signals == nil {
		w.Signals = []HealthSignal{}
	}
	return json.Marshal(w)
}
