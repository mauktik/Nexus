// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nginx

import (
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/platform"
)

// The distributions' include directories, used when includeDir is empty
// (ASM-3, L-2): Alpine's nginx.conf includes /etc/nginx/http.d/*.conf in its
// http block, Debian's and Ubuntu's include /etc/nginx/conf.d/*.conf.
const (
	AlpineIncludeDir  = "/etc/nginx/http.d"
	DefaultIncludeDir = "/etc/nginx/conf.d"
)

// Settings are the nginx section in force, with IncludeDir resolved to the
// directory actually used. The resolved value lives only here: it is never
// written back into the configuration.
type Settings config.NginxConfig

// effectiveSettings resolves an empty IncludeDir by the host's OS. Only the
// platform.OS ID decides, never the init system: Debian booted with OpenRC
// still has Debian's nginx package layout.
func effectiveSettings(c config.NginxConfig, os platform.OS) Settings {
	s := Settings(c)
	if s.IncludeDir == "" {
		s.IncludeDir = DefaultIncludeDir
		if os.IsAlpine() {
			s.IncludeDir = AlpineIncludeDir
		}
	}
	return s
}

// EnabledFile is the file name of site name while enabled: the include
// glob <includeDir>/*<includeSuffix> matches it.
func (s Settings) EnabledFile(name string) string { return name + s.IncludeSuffix }

// DisabledFile is the file name of site name while disabled, which the
// include glob does not match.
func (s Settings) DisabledFile(name string) string { return name + s.IncludeSuffix + s.DisabledSuffix }
