package apitypes

import (
	"encoding/json"
	"time"

	"github.com/mauktik/nexus/internal/problem"
)

// OperationStatus is the lifecycle state of an operation (FR-4).
type OperationStatus string

const (
	StatusQueued    OperationStatus = "Queued"
	StatusRunning   OperationStatus = "Running"
	StatusSucceeded OperationStatus = "Succeeded"
	StatusFailed    OperationStatus = "Failed"
	StatusCanceled  OperationStatus = "Canceled"
)

// Terminal reports whether s is a final state.
func (s OperationStatus) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCanceled:
		return true
	}
	return false
}

// Operation is the wire form of a long-running operation (FR-4). It holds only
// what clients see; store.OpRecord holds the internal fields and maps to this
// explicitly. Every key is always present, so the shape does not vary by state.
type Operation struct {
	ID        string          `json:"id"`
	Host      string          `json:"host"`
	Action    string          `json:"action"` // Op ID, e.g. "docker.containers.restart"
	Provider  string          `json:"provider"`
	Target    string          `json:"target"` // Resource ID, or empty for a provider action
	TokenName string          `json:"tokenName"`
	Status    OperationStatus `json:"status"`
	Created   time.Time       `json:"created"`
	Started   *time.Time      `json:"started"`
	Ended     *time.Time      `json:"ended"`
	// Result is the op's output on success. Mutating ops embed ActionResult,
	// so it carries "changed".
	Result        json.RawMessage `json:"result"`
	Error         *problem.Error  `json:"error"` // Structured error on failure
	OutputEvicted bool            `json:"outputEvicted"`
}

// ActionResult is the common part of a mutating operation's result. Provider
// output types embed it.
type ActionResult struct {
	Changed  bool     `json:"changed"` // False when the target was already in the requested state
	Warnings []string `json:"warnings,omitempty"`
}
