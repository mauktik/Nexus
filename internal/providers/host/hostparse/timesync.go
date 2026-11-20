// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package hostparse

import (
	"errors"
	"strings"
)

// TimedatectlNTPSynchronized parses the output of
// `timedatectl show --property=NTPSynchronized --value`, a systemd boolean
// printed as "yes" or "no". Anything else is an error.
func TimedatectlNTPSynchronized(out []byte) (bool, error) {
	switch strings.TrimSpace(string(out)) {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	}
	return false, errors.New("timedatectl: NTPSynchronized is not yes or no")
}

// chronyLeapField is the 1-based position of the leap status in
// `chronyc -c tracking`: Reference ID, its address or name, Stratum, Ref
// time, System time, Last offset, RMS offset, Frequency, Residual freq,
// Skew, Root delay, Root dispersion, Update interval, Leap status (chrony
// client.c process_cmd_tracking; -c prints each field comma-separated).
const chronyLeapField = 14

// ChronyTracking parses the first line of `chronyc -c tracking` and reports
// whether chronyd is synchronised from the leap status (chrony client.c
// print_report %L): "Normal", "Insert second" and "Delete second" are
// synchronised, "Not synchronised" is not. Another value, or a line with
// fewer than 14 fields, is an error.
func ChronyTracking(out []byte) (bool, error) {
	var first string
	for l := range lines(out) {
		first = l
		break
	}
	f := strings.Split(first, ",")
	if len(f) < chronyLeapField {
		return false, errors.New("chronyc tracking: fewer than 14 fields")
	}
	switch f[chronyLeapField-1] {
	case "Normal", "Insert second", "Delete second":
		return true, nil
	case "Not synchronised":
		return false, nil
	}
	return false, errors.New("chronyc tracking: unknown leap status")
}
