package nexusd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/config"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// configList returns every leaf key of the configuration file at the
// daemon's ConfigPath, in schema order (F4). It reads the file on every call:
// the values are the file's, never the configuration in force. Sensitive
// values arrive from config.InspectAll already redacted.
func (p *Provider) configList(context.Context, spec.ConfigListInput) (apitypes.List[spec.ConfigKey], error) {
	var none apitypes.List[spec.ConfigKey]
	path, data, err := p.readConfigFile()
	if err != nil {
		return none, err
	}
	entries, err := config.InspectAll(data, path)
	if err != nil {
		return none, configReadFailed(path, err)
	}
	items := make([]spec.ConfigKey, 0, len(entries))
	for _, e := range entries {
		items = append(items, p.configKey(e))
	}
	return apitypes.List[spec.ConfigKey]{Items: items}, nil
}

// configGet returns one key, a leaf or a section, of the configuration file
// at the daemon's ConfigPath (F4). An unknown key is not-found naming its
// nearest valid parent and that parent's keys.
func (p *Provider) configGet(_ context.Context, in spec.ConfigGetInput) (spec.ConfigKey, error) {
	var none spec.ConfigKey
	path, data, err := p.readConfigFile()
	if err != nil {
		return none, err
	}
	e, err := config.Inspect(data, path, in.Name)
	if err != nil {
		var unknown *config.UnknownKeyError
		if errors.As(err, &unknown) {
			return none, problem.New(problem.NotFound, unknownKeyDetail(unknown))
		}
		return none, configReadFailed(path, err)
	}
	return p.configKey(e), nil
}

// readConfigFile reads the file at the daemon's ConfigPath. A failure is
// config-invalid carrying the path.
func (p *Provider) readConfigFile() (string, []byte, error) {
	d, err := p.daemonOf()
	if err != nil {
		return "", nil, err
	}
	path := d.ConfigPath()
	data, err := config.ReadFile(path)
	if err != nil {
		return path, nil, configReadFailed(path, err)
	}
	return path, data, nil
}

// configReadFailed maps a read or parse failure of the file at path to
// config-invalid with its located errors. An error that is not an ErrorList
// becomes a single error naming the file.
func configReadFailed(path string, err error) error {
	var list config.ErrorList
	if !errors.As(err, &list) || len(list) == 0 {
		list = config.ErrorList{{File: path, Msg: err.Error()}}
	}
	if perr := configValid(path, list); perr != nil {
		return perr
	}
	return fmt.Errorf("nexusd: read %s: %w", path, err)
}

// unknownKeyDetail is the not-found detail of an unknown key: "no key K;"
// then its nearest valid parent and that parent's keys, or the top-level
// keys when no part of K is a section.
func unknownKeyDetail(e *config.UnknownKeyError) string {
	keys := strings.Join(e.Children, ", ")
	if e.Parent == "" {
		return fmt.Sprintf("no key %s; the top-level keys are: %s", e.Path, keys)
	}
	return fmt.Sprintf("no key %s; the nearest valid parent is %s, whose keys are: %s", e.Path, e.Parent, keys)
}

// configKey is the resource of an inspected key.
func (p *Provider) configKey(e config.Entry) spec.ConfigKey {
	return apitypes.NewResource(p.host, spec.Name, spec.ConfigType, e.Key, configKeyProperties(e))
}

// configKeyProperties converts an entry to the wire shape. A section's
// value maps each child key to that child's own value, effective value and
// source.
func configKeyProperties(e config.Entry) spec.ConfigKeyProperties {
	value := e.Value
	if children, ok := e.Value.(map[string]config.Entry); ok {
		m := make(map[string]spec.ConfigKeyProperties, len(children))
		for name, c := range children {
			m[name] = configKeyProperties(c)
		}
		value = m
	}
	return spec.ConfigKeyProperties{Value: value, Effective: e.Effective, Source: e.Source}
}
