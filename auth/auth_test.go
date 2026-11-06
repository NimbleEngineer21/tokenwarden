package auth

import (
	"errors"
	"strings"
	"testing"
)

const testKey = "tw_test_alice"

func TestHashKey(t *testing.T) {
	// printf %s tw_test_alice | shasum -a 256
	const want = "0de84bb906b982095f7af3724412c2c2481eee3ffb8469a18c0696c608b56227"
	if got := HashKey(testKey); got != want {
		t.Fatalf("HashKey = %q, want %q", got, want)
	}
}

func TestStaticAuthenticate(t *testing.T) {
	s, err := NewStatic([]StaticKey{{SHA256: HashKey(testKey), Team: "red", User: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Authenticate(t.Context(), testKey)
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{KeyID: HashKey(testKey)[:keyIDLen], Team: "red", User: "alice"}
	if id != want {
		t.Fatalf("Authenticate = %+v, want %+v", id, want)
	}
	if _, err := s.Authenticate(t.Context(), "tw_test_unknown"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown key: err = %v, want ErrUnauthorized", err)
	}
	if _, err := s.Authenticate(t.Context(), ""); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("empty key: err = %v, want ErrUnauthorized", err)
	}
}

func TestNewStaticRejects(t *testing.T) {
	h := HashKey(testKey)
	tests := []struct {
		name string
		keys []StaticKey
		want string
	}{
		{"short hash", []StaticKey{{SHA256: "abc", Team: "red"}}, "64 lowercase hex"},
		{"upper hash", []StaticKey{{SHA256: strings.ToUpper(h), Team: "red"}}, "64 lowercase hex"},
		{"raw key", []StaticKey{{SHA256: testKey, Team: "red"}}, "64 lowercase hex"},
		{"no team", []StaticKey{{SHA256: h}}, "team is required"},
		{"duplicate", []StaticKey{{SHA256: h, Team: "red"}, {SHA256: h, Team: "blue"}}, "duplicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewStatic(tt.keys)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestLoadStatic(t *testing.T) {
	h := HashKey(testKey)
	t.Run("valid", func(t *testing.T) {
		s, err := LoadStatic(strings.NewReader(`{"keys":[{"sha256":"` + h + `","team":"red"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.Authenticate(t.Context(), testKey)
		if err != nil || id.Team != "red" || id.User != "" {
			t.Fatalf("Authenticate = %+v, %v", id, err)
		}
	})
	for name, in := range map[string]string{
		"unknown field": `{"keys":[{"key":"` + testKey + `","team":"red"}]}`,
		"trailing data": `{"keys":[]} {}`,
		"not json":      `keys`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadStatic(strings.NewReader(in)); err == nil {
				t.Fatal("err = nil, want an error")
			}
		})
	}
}
