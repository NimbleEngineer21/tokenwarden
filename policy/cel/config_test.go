package cel

import (
	"strings"
	"testing"
)

func TestParseFileErrors(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"empty", "", "policy file is empty"},
		{"no version", "rules: []", "version 0 is not supported"},
		{"future version", "version: 2", "version 2 is not supported"},
		{"bad mode", "version: 1\nmode: audit", `mode "audit"`},
		{"unknown field", "version: 1\nrulez: []", "field rulez not found"},
		{"unknown rule field", "version: 1\nrules:\n- name: a\n  effect: allow\n  when: x", "field when not found"},
		{"no name", "version: 1\nrules:\n- effect: allow", "rule 1 \"\": name is empty"},
		{"bad effect", "version: 1\nrules:\n- name: a\n  effect: permit", `effect "permit"`},
		{"no effect", "version: 1\nrules:\n- name: a", `effect ""`},
		{"deny without reason", "version: 1\nrules:\n- name: a\n  effect: deny", "a deny rule needs a reason"},
		{
			"duplicate name",
			"version: 1\nrules:\n- name: a\n  effect: allow\n- name: a\n  effect: allow",
			`rule 2 "a": the name is used by an earlier rule`,
		},
		{"empty team", "version: 1\nrules:\n- name: a\n  effect: allow\n  match: {teams: ['']}", "empty team or user"},
		{"empty user", "version: 1\nrules:\n- name: a\n  effect: allow\n  match: {users: ['']}", "empty team or user"},
		{"not yaml", "version: [", "yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseFile([]byte(tt.yaml))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("parseFile() error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestParseFileModeDefaultsToEnforce(t *testing.T) {
	f, err := parseFile([]byte("version: 1\nrules: []"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Mode != ModeEnforce {
		t.Fatalf("Mode = %q, want %q", f.Mode, ModeEnforce)
	}
}

func TestMatchSpec(t *testing.T) {
	tests := []struct {
		name       string
		m          matchSpec
		team, user string
		want       bool
	}{
		{"empty matches any", matchSpec{}, "red", "ann", true},
		{"team in list", matchSpec{Teams: []string{"blue", "red"}}, "red", "ann", true},
		{"team not in list", matchSpec{Teams: []string{"blue"}}, "red", "ann", false},
		{"user in list", matchSpec{Users: []string{"ann"}}, "red", "ann", true},
		{"user not in list", matchSpec{Users: []string{"bob"}}, "red", "ann", false},
		{"both must match", matchSpec{Teams: []string{"red"}, Users: []string{"bob"}}, "red", "ann", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.m.matches(tt.team, tt.user); got != tt.want {
				t.Fatalf("matches(%q, %q) = %v, want %v", tt.team, tt.user, got, tt.want)
			}
		})
	}
}
