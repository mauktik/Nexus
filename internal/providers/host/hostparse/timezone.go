// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package hostparse

import (
	"errors"
	"strings"
	"time"
)

// Timezone sources.
const (
	TZSourceTimezoneFile = "/etc/timezone"
	TZSourceLocaltime    = "/etc/localtime"
	TZSourceLink         = "/etc/localtime link"
	TZSourceDefault      = "default (no /etc/localtime)"
)

// TimezoneInfo is the host's zone (A-31). Name is "" when not reported, with
// NameSource "" too. Offset (seconds east of UTC) and Abbrev are the zone's
// at the time asked for; Source says where they came from.
type TimezoneInfo struct {
	Name       string
	NameSource string
	Offset     int
	Abbrev     string
	Source     string
}

// Timezone resolves the host's zone from /etc/localtime's bytes (nil when
// the file is absent), /etc/timezone's bytes (nil when absent) and
// /etc/localtime's link target ("" when it is not a link).
//
// The name is /etc/timezone's first line, else the link target after its
// last "zoneinfo/", else not reported; a value that is not a plausible zone
// name is ignored. The offset is /etc/localtime parsed as a tzfile
// (tzfile(5), time.LoadLocationFromTZData) at now. Without /etc/localtime
// the zone is UTC, offset 0, whatever /etc/timezone says, since the C
// library then runs in UTC (A-31). When /etc/localtime
// does not parse, the error is returned with the name fields still set.
func Timezone(localtime, etcTimezone []byte, link string, now time.Time) (TimezoneInfo, error) {
	if localtime == nil {
		return TimezoneInfo{Name: "UTC", NameSource: TZSourceDefault, Abbrev: "UTC", Source: TZSourceDefault}, nil
	}
	var tz TimezoneInfo
	first, _, _ := strings.Cut(string(etcTimezone), "\n")
	if n := strings.TrimSpace(first); validZoneName(n) {
		tz.Name, tz.NameSource = n, TZSourceTimezoneFile
	} else if i := strings.LastIndex(link, "zoneinfo/"); i >= 0 && validZoneName(link[i+len("zoneinfo/"):]) {
		tz.Name, tz.NameSource = link[i+len("zoneinfo/"):], TZSourceLink
	}
	loc, err := time.LoadLocationFromTZData("localtime", localtime)
	if err != nil {
		return tz, errors.New("etc/localtime: not a tzfile")
	}
	tz.Abbrev, tz.Offset = now.In(loc).Zone()
	tz.Source = TZSourceLocaltime
	return tz, nil
}

// validZoneName accepts a relative tz database name such as
// "America/New_York", "Etc/GMT+5" or "UTC": ASCII letters, digits and
// "/_+-.", no empty, "." or ".." element, at most 255 bytes.
func validZoneName(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for _, c := range []byte(s) {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte("/_+-.", c) >= 0
		if !ok {
			return false
		}
	}
	for e := range strings.SplitSeq(s, "/") {
		if e == "" || e == "." || e == ".." {
			return false
		}
	}
	return true
}
