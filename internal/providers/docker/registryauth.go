package docker

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/moby/moby/api/types/registry"
)

// Registry credentials (A-14, A-61). Only the auths section of
// <configDir>/config.json is read for credentials. credHelpers and
// credsStore are read only to name, in a reason, the helper that holds a
// credential Nexus cannot see: a credential helper is never executed.
//
// The file format is the Docker CLI's (docker/cli
// cli/config/configfile/file.go: `AuthConfigs map[string]types.AuthConfig
// json:"auths"`, `CredentialsStore string json:"credsStore"`,
// `CredentialHelpers map[string]string json:"credHelpers"`; an auths entry's
// "auth" is base64 of "user:password", decoded by decodeAuth).

// configFileName is the Docker CLI's configuration file in a config dir.
const configFileName = "config.json"

// maxConfigBytes bounds the read of config.json.
const maxConfigBytes = 1 << 20

// hubAuthKey is the auths key of Docker Hub (docker/cli configfile:
// `const authConfigKey = "https://index.docker.io/v1/"`).
const hubAuthKey = "https://index.docker.io/v1/"

// hubDomain is the domain reference.Domain reports for a Docker Hub image.
const hubDomain = "docker.io"

// hubHostnames are the hostnames that mean Docker Hub in an auths or
// credHelpers key.
var hubHostnames = []string{"docker.io", "index.docker.io", "registry-1.docker.io"}

// configAuth is one auths entry. The CLI writes an empty entry for a
// registry whose credential lives in a credential store.
type configAuth struct {
	Auth          string `json:"auth,omitempty"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	IdentityToken string `json:"identitytoken,omitempty"`
	RegistryToken string `json:"registrytoken,omitempty"`
}

func (a configAuth) empty() bool {
	return a.Auth == "" && a.Username == "" && a.Password == "" && a.IdentityToken == "" && a.RegistryToken == ""
}

// registryConfig is what config.json says about registries.
type registryConfig struct {
	path        string
	auths       map[string]configAuth
	credsStore  string
	credHelpers map[string]string
}

// registryCreds is the outcome of the lookup for one registry.
type registryCreds struct {
	// key is the auths key the credential came from; "" when there is
	// none and the registry is queried anonymously.
	key string
	// encoded is the X-Registry-Auth value: base64url JSON of the
	// credential, sent to the Engine for this registry's reference only.
	encoded string
	// helper is the credential helper config.json names for the registry
	// ("docker-credential-<name>"), from credHelpers or else credsStore.
	// It is never executed.
	helper string
	// helperSource is "credHelpers" or "credsStore".
	helperSource string
}

// loadRegistryConfig reads <dir>/config.json. A missing file or directory is
// an empty configuration: every registry is then queried anonymously. Any
// other failure is an error, and every registry check then ends unknown.
// The error never quotes the file's content.
func loadRegistryConfig(dir string) (*registryConfig, error) {
	path := filepath.Join(dir, configFileName)
	cfg := &registryConfig{path: path}
	f, err := os.Open(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return cfg, nil
	case err != nil:
		return cfg, fmt.Errorf("cannot open %s: %w", path, unwrapPath(err))
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return cfg, fmt.Errorf("cannot read %s: %w", path, unwrapPath(err))
	}
	if len(b) > maxConfigBytes {
		return cfg, fmt.Errorf("%s is larger than %d bytes", path, maxConfigBytes)
	}
	var file struct {
		Auths       map[string]configAuth `json:"auths"`
		CredsStore  string                `json:"credsStore"`
		CredHelpers map[string]string     `json:"credHelpers"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		// A syntax error's text carries no content; its offset is enough.
		var syn *json.SyntaxError
		if errors.As(err, &syn) {
			return cfg, fmt.Errorf("%s is not valid JSON (at byte %d)", path, syn.Offset)
		}
		return cfg, fmt.Errorf("%s does not have the Docker CLI's config.json shape", path)
	}
	cfg.auths, cfg.credsStore, cfg.credHelpers = file.Auths, file.CredsStore, file.CredHelpers
	return cfg, nil
}

// unwrapPath drops the path from a *fs.PathError, which the caller names.
func unwrapPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// forRegistry returns the credential for registry domain (reference.Domain
// of the image reference: "docker.io" for Docker Hub, else host[:port]). A
// credential is chosen only from an auths key that names exactly this
// registry, so it is never sent for another one.
func (c *registryConfig) forRegistry(domain string) (registryCreds, error) {
	var out registryCreds
	out.helper, out.helperSource = c.helperFor(domain)
	key, ok := matchKey(mapKeys(c.auths), domain)
	if !ok {
		return out, nil
	}
	entry := c.auths[key]
	if entry.empty() {
		return out, nil // Written by docker login with a credential store.
	}
	ac := registry.AuthConfig{
		Username:      entry.Username,
		Password:      entry.Password,
		IdentityToken: entry.IdentityToken,
		RegistryToken: entry.RegistryToken,
		ServerAddress: key,
	}
	if entry.Auth != "" {
		user, pass, err := decodeAuth(entry.Auth)
		if err != nil {
			return out, fmt.Errorf("the auths entry %q in %s: %w", key, c.path, err)
		}
		ac.Username, ac.Password = user, pass
	}
	js, err := json.Marshal(ac)
	if err != nil {
		return out, fmt.Errorf("encode the credential for %s: %w", key, err)
	}
	// The Engine API's X-Registry-Auth is base64url JSON (Engine API v1.52,
	// "Authentication").
	out.key, out.encoded = key, base64.URLEncoding.EncodeToString(js)
	return out, nil
}

// helperFor is the credential helper config.json names for domain: its
// credHelpers entry, else credsStore (docker/cli configfile
// getConfiguredCredentialStore).
func (c *registryConfig) helperFor(domain string) (name, source string) {
	if key, ok := matchKey(mapKeys(c.credHelpers), domain); ok && c.credHelpers[key] != "" {
		return "docker-credential-" + c.credHelpers[key], "credHelpers"
	}
	if c.credsStore != "" {
		return "docker-credential-" + c.credsStore, "credsStore"
	}
	return "", ""
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// matchKey finds the key of keys that names registry domain: an exact match
// first, then a key whose hostname (scheme and path removed, as the Docker
// CLI's ConvertToHostname does) is the domain. Docker Hub matches its
// historical key and its hostnames. keys are sorted, so the choice is
// stable.
func matchKey(keys []string, domain string) (string, bool) {
	hub := domain == hubDomain
	if hub && slices.Contains(keys, hubAuthKey) {
		return hubAuthKey, true
	}
	if slices.Contains(keys, domain) {
		return domain, true
	}
	for _, k := range keys {
		h := hostnameOf(k)
		if h == domain || (hub && slices.Contains(hubHostnames, h)) {
			return k, true
		}
	}
	return "", false
}

// hostnameOf reduces an auths key such as "https://host:5000/v1/" to
// "host:5000" (docker/cli cli/config/credentials/file_store.go,
// ConvertToHostname).
func hostnameOf(key string) string {
	if _, rest, ok := strings.Cut(key, "://"); ok {
		key = rest
	}
	host, _, _ := strings.Cut(key, "/")
	return strings.ToLower(host)
}

// decodeAuth splits a base64 "user:password" auth value, as the Docker CLI's
// decodeAuth does. Errors never quote the value.
func decodeAuth(s string) (user, pass string, err error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", "", errors.New(`"auth" is not base64`)
	}
	user, pass, ok := strings.Cut(string(b), ":")
	if !ok || user == "" {
		return "", "", errors.New(`"auth" is not base64 of user:password`)
	}
	return user, strings.Trim(pass, "\x00"), nil
}
