package timemachine

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	// ReasonBinaryPlist: the history plist is a binary property list,
	// which is not parsed (ASM-8, A-15, CON-2).
	ReasonBinaryPlist = "binary plist not parsed"
	// ReasonNoCompletionDate: the XML plist holds no completion date.
	ReasonNoCompletionDate = "no SnapshotCompletionDate"
)

// completionKey is the key whose following <date> is a snapshot's
// completion time in com.apple.TimeMachine.SnapshotHistory.plist.
const completionKey = "com.apple.backupd.SnapshotCompletionDate"

// binaryMagic starts every binary property list ("bplist00", or a later
// version digit pair).
var binaryMagic = []byte("bplist")

// parseReason is the reason for a document encoding/xml refused. It never
// quotes the error: encoding/xml puts element and entity names from the
// document, which the share's users write, into its messages, with no
// length limit (B-SEC-1).
func parseReason(err error) string {
	var se *xml.SyntaxError
	if errors.As(err, &se) {
		return fmt.Sprintf("plist not parsed: XML syntax error on line %d", se.Line)
	}
	return "plist not parsed: not a well-formed XML plist"
}

// LastCompletion returns the latest <date> that immediately follows a
// <key>com.apple.backupd.SnapshotCompletionDate</key> anywhere in an XML
// plist, in UTC, or nil with the reason: ReasonBinaryPlist for a binary
// plist, ReasonNoCompletionDate when no such pair is present, or a parse
// reason for a document encoding/xml refuses or a date that is not an
// ISO 8601 plist date.
//
// The document is untrusted. encoding/xml never resolves a DOCTYPE: an
// internal or external entity it declares is never expanded or fetched,
// and a reference to one is a parse error.
func LastCompletion(plist []byte) (*time.Time, string) {
	if bytes.HasPrefix(plist, binaryMagic) {
		return nil, ReasonBinaryPlist
	}
	d := xml.NewDecoder(bytes.NewReader(plist))
	d.Strict = true
	var latest *time.Time
	afterKey := false
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, parseReason(err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch {
		case se.Name.Local == "key":
			var k string
			if err := d.DecodeElement(&k, &se); err != nil {
				return nil, parseReason(err)
			}
			afterKey = strings.TrimSpace(k) == completionKey
			continue
		case se.Name.Local == "date" && afterKey:
			var s string
			if err := d.DecodeElement(&s, &se); err != nil {
				return nil, parseReason(err)
			}
			t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
			if err != nil {
				return nil, fmt.Sprintf("plist not parsed: %s is not a plist date", completionKey)
			}
			if t = t.UTC(); latest == nil || t.After(*latest) {
				latest = &t
			}
		}
		afterKey = false
	}
	if latest == nil {
		return nil, ReasonNoCompletionDate
	}
	return latest, ""
}
