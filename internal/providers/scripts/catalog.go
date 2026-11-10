package scripts

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mauktik/nexus/internal/config"
)

// Catalog entry defaults (FR-18, A-11).
const (
	DefaultTimeout          = time.Hour
	DefaultConcurrency      = 1
	DefaultTerminationGrace = 30 * time.Second
)

// Script is one catalog entry with its defaults applied.
type Script struct {
	ID               string
	Description      string
	Path             string // Absolute
	RunAs            string
	WorkingDirectory string // Absolute
	Timeout          time.Duration
	Concurrency      int
	TerminationGrace time.Duration // Between SIGTERM and SIGKILL
	Env              map[string]string
	Parameters       []Parameter // In declared order
}

// Parameter is one declared script parameter.
type Parameter struct {
	Name      string
	Type      string
	Pattern   string
	Enum      []string // Nil: any value of the type
	Sensitive bool
	Arg       string
	Env       string
	// Position is the 1-based position among the positional parameters, the
	// ones with neither Arg nor Env, in declared order. Zero otherwise.
	Position int

	re *regexp.Regexp // Pattern, compiled; nil when there is none
}

// EnvNames returns the script's environment variable names, sorted. The
// values can be secrets and are never returned to a client (NFR-4).
func (s Script) EnvNames() []string {
	names := slices.AppendSeq(make([]string, 0, len(s.Env)), maps.Keys(s.Env))
	slices.Sort(names)
	return names
}

func (s Script) clone() Script {
	s.Env = maps.Clone(s.Env)
	s.Parameters = slices.Clone(s.Parameters)
	for i := range s.Parameters {
		s.Parameters[i].Enum = slices.Clone(s.Parameters[i].Enum)
	}
	return s
}

// Catalog is an immutable set of scripts, sorted by ID. A reload builds a new
// one and swaps it in whole, so a reader holding one sees a single version.
type Catalog struct {
	scripts []Script
	index   map[string]int
}

// NewCatalog validates entries with the validate-config rules and applies
// the defaults. Every problem is reported, each naming its entry and field.
func NewCatalog(entries []config.Script) (*Catalog, error) {
	var errs []error
	config.CheckCatalog(entries, func(path, msg string) {
		errs = append(errs, fmt.Errorf("%s: %s", path, msg))
	})
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	c := &Catalog{scripts: make([]Script, 0, len(entries)), index: make(map[string]int, len(entries))}
	for _, e := range entries {
		// A zero in the file means the default.
		s := Script{
			ID:               e.ID,
			Description:      e.Description,
			Path:             e.Path,
			RunAs:            e.RunAs,
			WorkingDirectory: e.WorkingDirectory,
			Timeout:          cmp.Or(e.Timeout, DefaultTimeout),
			Concurrency:      cmp.Or(e.Concurrency, DefaultConcurrency),
			TerminationGrace: cmp.Or(e.TerminationGrace, DefaultTerminationGrace),
			Env:              maps.Clone(e.Env),
			Parameters:       make([]Parameter, 0, len(e.Parameters)),
		}
		if s.Env == nil {
			s.Env = map[string]string{}
		}
		positional := 0
		for _, p := range e.Parameters {
			prm := Parameter{Name: p.Name, Type: p.Type, Pattern: p.Pattern, Enum: slices.Clone(p.Enum),
				Sensitive: p.Sensitive, Arg: p.Arg, Env: p.Env}
			if p.Pattern != "" {
				// The pattern decides the whole value, not a substring of it:
				// an unanchored one would let "--upload-pack=x" through a
				// pattern meant to keep out leading dashes. CheckCatalog
				// compiled it already, and a valid pattern stays valid when
				// wrapped. Pattern stays as declared, for the reads.
				prm.re = regexp.MustCompile(anchored(p.Pattern))
			}
			if p.Arg == "" && p.Env == "" {
				positional++
				prm.Position = positional
			}
			s.Parameters = append(s.Parameters, prm)
		}
		c.scripts = append(c.scripts, s)
	}
	slices.SortFunc(c.scripts, func(a, b Script) int { return strings.Compare(a.ID, b.ID) })
	for i, s := range c.scripts {
		c.index[s.ID] = i
	}
	return c, nil
}

// anchored is the pattern that matches only a value the whole of which
// matches pattern.
func anchored(pattern string) string { return "^(?:" + pattern + ")$" }

// decodeCatalog strictly decodes a scripts section and builds its catalog.
// An absent section is an empty catalog.
func decodeCatalog(decode func(any) error) (*Catalog, error) {
	settings := config.ScriptsConfig{Catalog: []config.Script{}}
	if err := decode(&settings); err != nil {
		return nil, err
	}
	return NewCatalog(settings.Catalog)
}

// Get returns a copy of the script with id.
func (c *Catalog) Get(id string) (Script, bool) {
	i, ok := c.index[id]
	if !ok {
		return Script{}, false
	}
	return c.scripts[i].clone(), true
}

// Scripts returns copies of every script, sorted by ID.
func (c *Catalog) Scripts() []Script {
	out := make([]Script, len(c.scripts))
	for i, s := range c.scripts {
		out[i] = s.clone()
	}
	return out
}

// after returns copies of at most limit scripts whose IDs sort after id,
// and whether more follow.
func (c *Catalog) after(id string, limit int) ([]Script, bool) {
	start, _ := slices.BinarySearchFunc(c.scripts, id, func(s Script, id string) int { return strings.Compare(s.ID, id) })
	if start < len(c.scripts) && c.scripts[start].ID == id {
		start++
	}
	end := min(start+limit, len(c.scripts))
	out := make([]Script, 0, end-start)
	for _, s := range c.scripts[start:end] {
		out = append(out, s.clone())
	}
	return out, end < len(c.scripts)
}
