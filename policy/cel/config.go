package cel

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"

	"go.yaml.in/yaml/v3"
)

// Mode says whether an [Engine] enforces its decisions.
type Mode string

const (
	// ModeEnforce denies the requests that the rules deny.
	ModeEnforce Mode = "enforce"
	// ModeObserve allows every request and logs the requests that the
	// rules would deny.
	ModeObserve Mode = "observe"
)

// Effect is what a rule does to a request that it matches.
type Effect string

const (
	// EffectAllow allows the request unless a deny rule also matches.
	EffectAllow Effect = "allow"
	// EffectDeny denies the request.
	EffectDeny Effect = "deny"
)

// fileVersion is the only policy file version that this package reads.
const fileVersion = 1

type fileSpec struct {
	Version int        `yaml:"version"`
	Mode    Mode       `yaml:"mode"`
	Rules   []ruleSpec `yaml:"rules"`
}

type ruleSpec struct {
	Name   string    `yaml:"name"`
	Match  matchSpec `yaml:"match"`
	Expr   string    `yaml:"expr"`
	Effect Effect    `yaml:"effect"`
	Reason string    `yaml:"reason"`
}

// matchSpec selects requests by team and user before the expression runs.
// An empty list matches any value.
type matchSpec struct {
	Teams []string `yaml:"teams"`
	Users []string `yaml:"users"`
}

func (m matchSpec) matches(team, user string) bool {
	return (len(m.Teams) == 0 || slices.Contains(m.Teams, team)) &&
		(len(m.Users) == 0 || slices.Contains(m.Users, user))
}

// parseFile decodes and checks a policy file. It does not compile the
// expressions.
func parseFile(data []byte) (fileSpec, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var f fileSpec
	if err := dec.Decode(&f); err != nil {
		if errors.Is(err, io.EOF) {
			return fileSpec{}, errors.New("policy file is empty")
		}
		return fileSpec{}, err
	}
	if f.Version != fileVersion {
		return fileSpec{}, fmt.Errorf("version %d is not supported; use version %d", f.Version, fileVersion)
	}
	switch f.Mode {
	case "":
		f.Mode = ModeEnforce
	case ModeEnforce, ModeObserve:
	default:
		return fileSpec{}, fmt.Errorf("mode %q is not %q or %q", f.Mode, ModeEnforce, ModeObserve)
	}
	seen := make(map[string]bool, len(f.Rules))
	for i, r := range f.Rules {
		if err := r.check(); err != nil {
			return fileSpec{}, fmt.Errorf("rule %d %q: %w", i+1, r.Name, err)
		}
		if seen[r.Name] {
			return fileSpec{}, fmt.Errorf("rule %d %q: the name is used by an earlier rule", i+1, r.Name)
		}
		seen[r.Name] = true
	}
	return f, nil
}

func (r ruleSpec) check() error {
	if r.Name == "" {
		return errors.New("name is empty")
	}
	switch r.Effect {
	case EffectAllow:
	case EffectDeny:
		// The reason of a deny goes back to the client, so it must exist.
		if r.Reason == "" {
			return errors.New("a deny rule needs a reason")
		}
	default:
		return fmt.Errorf("effect %q is not %q or %q", r.Effect, EffectAllow, EffectDeny)
	}
	if slices.Contains(r.Match.Teams, "") || slices.Contains(r.Match.Users, "") {
		return errors.New("match has an empty team or user")
	}
	return nil
}
