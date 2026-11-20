// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package provider

import "github.com/mauktik/nexus/internal/apitypes"

// ActiveOperation is an operation that has not ended: Queued or Running.
type ActiveOperation struct {
	ID     string                   // Operation ID
	Op     string                   // Op ID
	Status apitypes.OperationStatus // apitypes.StatusQueued or apitypes.StatusRunning
}

// Operations is the operation manager's view of non-terminal operations.
type Operations interface {
	// Active lists the Queued and Running operations.
	Active() ([]ActiveOperation, error)
}

// OperationsUser is a provider that needs the operation manager's view of
// non-terminal operations. The daemon calls UseOperations once, before
// serving any request.
type OperationsUser interface {
	UseOperations(Operations)
}
