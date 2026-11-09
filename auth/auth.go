// Package auth identifies the caller of the gateway from its gateway key.
//
// The gateway never stores or logs a raw key. A store holds the SHA-256 hash
// of each key, and [Identity.KeyID] is a short prefix of that hash that is
// safe to log.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

// ErrUnauthorized means that the key is not known to the store. An
// [Authenticator] returns it for a valid-looking key that matches nothing, so
// that the gateway can tell a bad key from a store failure.
var ErrUnauthorized = errors.New("auth: unknown gateway key")

// keyIDLen is the number of hex characters of the hash in a key ID: 48 bits,
// enough to tell keys apart in logs, too few to help an attacker.
const keyIDLen = 12

// Identity is the caller that a gateway key belongs to.
type Identity struct {
	// KeyID is a short prefix of the key hash. It is safe to log.
	KeyID string
	// Team owns the key. Budgets and policy apply per team.
	Team string
	// User is the person or service that holds the key. It can be empty.
	User string
}

// Authenticator maps a gateway key to an [Identity].
type Authenticator interface {
	// Authenticate returns the identity of key. It returns an error that
	// wraps [ErrUnauthorized] when key is not known.
	Authenticate(ctx context.Context, key string) (Identity, error)
}

// HashKey returns the lowercase hex SHA-256 of key, the form that a key
// store holds.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// Static is an in-memory [Authenticator] built from a fixed key list. It is
// safe for concurrent use.
type Static struct {
	byHash map[string]Identity
}

// StaticKey is one entry of a [Static] store.
type StaticKey struct {
	// SHA256 is the [HashKey] of the gateway key.
	SHA256 string `json:"sha256"`
	Team   string `json:"team"`
	User   string `json:"user,omitempty"`
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// NewStatic returns a store that holds keys. It returns an error for a
// malformed hash, an empty team, or a duplicate hash.
func NewStatic(keys []StaticKey) (*Static, error) {
	s := &Static{byHash: make(map[string]Identity, len(keys))}
	for i, k := range keys {
		switch {
		case !hashPattern.MatchString(k.SHA256):
			return nil, fmt.Errorf("auth: key %d: sha256 must be 64 lowercase hex characters", i)
		case k.Team == "":
			return nil, fmt.Errorf("auth: key %d: team is required", i)
		}
		if _, dup := s.byHash[k.SHA256]; dup {
			return nil, fmt.Errorf("auth: key %d: duplicate sha256", i)
		}
		s.byHash[k.SHA256] = Identity{KeyID: k.SHA256[:keyIDLen], Team: k.Team, User: k.User}
	}
	return s, nil
}

// keyFile is the JSON form that [LoadStatic] reads.
type keyFile struct {
	Keys []StaticKey `json:"keys"`
}

// LoadStatic reads a key file and returns its store. The file has the form
//
//	{"keys": [{"sha256": "<hex>", "team": "red", "user": "alice"}]}
//
// Unknown fields are an error, so that a raw key put in by mistake under a
// wrong field name is refused, not ignored.
func LoadStatic(r io.Reader) (*Static, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var f keyFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("auth: key file: %w", err)
	}
	if dec.More() {
		return nil, errors.New("auth: key file: data after the JSON object")
	}
	return NewStatic(f.Keys)
}

// Len returns the number of keys in the store.
func (s *Static) Len() int { return len(s.byHash) }

// Authenticate implements [Authenticator].
func (s *Static) Authenticate(_ context.Context, key string) (Identity, error) {
	// The lookup is by hash, so its timing tells nothing about a stored key.
	id, ok := s.byHash[HashKey(key)]
	if !ok {
		return Identity{}, ErrUnauthorized
	}
	return id, nil
}
