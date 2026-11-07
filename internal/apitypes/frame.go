package apitypes

import (
	"encoding/json"
	"time"
)

// Stream names carried in Frame.S.
const (
	StreamStdout = "out"
	StreamStderr = "err"
)

// FrameTypeTruncated marks a frame reporting output lost to the cap.
const FrameTypeTruncated = "truncated"

// Frame is one line of operation or container output, sent as NDJSON.
//
// A data frame is {"o":<absolute byte offset>,"t":"<RFC3339Nano>","s":"out"|"err","d":"<text>"}.
// Container logs have no byte offset, so their frames omit "o". A truncation
// marker is {"type":"truncated","lostBytes":K}, emitted when a reader asks for
// an offset older than the oldest retained byte. Frames carry neither host nor
// id: the CLI output layer adds those (FR-30).
type Frame struct {
	Type      string    `json:"type,omitempty"`
	O         *int64    `json:"o,omitempty"`
	T         time.Time `json:"t,omitzero"`
	S         string    `json:"s,omitempty"`
	D         string    `json:"d,omitempty"`
	LostBytes int64     `json:"lostBytes,omitempty"`
}

// DataFrame builds an offset-addressed output frame.
func DataFrame(offset int64, t time.Time, stream, data string) Frame {
	return Frame{O: &offset, T: t, S: stream, D: data}
}

// TruncatedFrame builds the marker for lostBytes of output dropped by the cap.
func TruncatedFrame(lostBytes int64) Frame {
	return Frame{Type: FrameTypeTruncated, LostBytes: lostBytes}
}

// Truncated reports whether f is a truncation marker.
func (f Frame) Truncated() bool { return f.Type == FrameTypeTruncated }

type dataFrameWire struct {
	O *int64    `json:"o,omitempty"`
	T time.Time `json:"t"`
	S string    `json:"s"`
	D string    `json:"d"`
}

type truncatedFrameWire struct {
	Type      string `json:"type"`
	LostBytes int64  `json:"lostBytes"`
}

// MarshalJSON writes exactly one of the two wire shapes. A data frame always
// carries t, s and d, even when d is empty; a marker carries only its two keys.
func (f Frame) MarshalJSON() ([]byte, error) {
	if f.Truncated() {
		return json.Marshal(truncatedFrameWire{Type: f.Type, LostBytes: f.LostBytes})
	}
	return json.Marshal(dataFrameWire{O: f.O, T: f.T, S: f.S, D: f.D})
}
