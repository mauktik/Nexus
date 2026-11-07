package apitypes

import "encoding/json"

// List is the response of every list operation.
type List[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor"` // Opaque; empty on the last page
}

type listWire[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor"`
}

// MarshalJSON writes an empty or nil Items as [], never null.
func (l List[T]) MarshalJSON() ([]byte, error) {
	items := l.Items
	if items == nil {
		items = []T{}
	}
	return json.Marshal(listWire[T]{Items: items, NextCursor: l.NextCursor})
}
