package nginx

import (
	"strings"
	"testing"

	"github.com/mauktik/nexus/internal/config"
)

// nginx-provider TC-71: The FR-12 name rule
// Owner WI 4.5. Requirements: FR-12, CON-4.
func TestValidName(t *testing.T) {
	def := Settings(config.Default().Providers.Nginx.Settings) // .conf and .disabled
	for _, name := range []string{"a", "example.com", "my_site-2", "A1", strings.Repeat("a", 128)} {
		if err := ValidName(name, def); err != nil {
			t.Errorf("ValidName(%q) = %v, want accepted", name, err)
		}
	}
	// Each refusal names the rule it broke: want is a fragment of the error.
	for _, tc := range []struct{ name, want string }{
		{"", "empty"},
		{".hidden", "start with a letter or digit"},
		{"-x", "start with a letter or digit"},
		{"_x", "start with a letter or digit"},
		{"a..b", `".."`},
		{strings.Repeat("a", 129), "128 characters"},
		{"a/b", "letters, digits"},
		{"a b", "letters, digits"},
		{"a\x00b", "letters, digits"},
		{"é", "start with a letter or digit"},
		{"xé", "letters, digits"},
		{"example.com.conf", "include suffix .conf"},
		{"x.disabled", "disabled suffix .disabled"},
		{"x.conf.disabled", "disabled suffix .disabled"},
		{"..", "start with a letter or digit"},
		{".", "start with a letter or digit"},
	} {
		err := ValidName(tc.name, def)
		if err == nil {
			t.Errorf("ValidName(%q) accepted, want refused (%s)", tc.name, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ValidName(%q) = %q, want it to name the rule %q", tc.name, err, tc.want)
		}
	}

	other := def
	other.IncludeSuffix, other.DisabledSuffix = ".site", ".off"
	if err := ValidName("x.conf", other); err != nil {
		t.Errorf("with .site/.off, ValidName(x.conf) = %v, want accepted", err)
	}
	if err := ValidName("x.site", other); err == nil || !strings.Contains(err.Error(), "include suffix .site") {
		t.Errorf("with .site/.off, ValidName(x.site) = %v, want refused naming the include suffix", err)
	}
	if err := ValidName("x.off", other); err == nil || !strings.Contains(err.Error(), "disabled suffix .off") {
		t.Errorf("with .site/.off, ValidName(x.off) = %v, want refused naming the disabled suffix", err)
	}
}
