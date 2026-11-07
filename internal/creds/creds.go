// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package creds resolves Kubernetes-style secret references ({name, key})
// to files. When flux4swarm runs as a Swarm service, the files are Docker
// secrets mounted under /run/secrets.
package creds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultDir is where Docker mounts secrets in a container.
const DefaultDir = "/run/secrets"

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("credential not found")

// Store reads credentials from a directory, with an optional in-memory
// overlay (used for command-line flags).
type Store struct {
	Dir     string
	Overlay map[string]map[string][]byte
}

// New returns a Store for dir.
func New(dir string) *Store {
	return &Store{Dir: dir, Overlay: map[string]map[string][]byte{}}
}

// Set adds an in-memory value.
func (s *Store) Set(name, key string, value []byte) {
	if s.Overlay == nil {
		s.Overlay = map[string]map[string][]byte{}
	}
	if s.Overlay[name] == nil {
		s.Overlay[name] = map[string][]byte{}
	}
	s.Overlay[name][key] = value
}

// Get returns the value of key in the secret name. It looks at
// <dir>/<name>/<key> and then <dir>/<name>_<key>.
func (s *Store) Get(name, key string) ([]byte, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, name, key)
	}
	if v, ok := s.Overlay[name][key]; ok {
		return v, nil
	}
	if err := validPart(name); err != nil {
		return nil, err
	}
	if err := validPart(key); err != nil {
		return nil, err
	}
	if s.Dir != "" {
		for _, p := range []string{
			filepath.Join(s.Dir, name, key),
			filepath.Join(s.Dir, name+"_"+key),
		} {
			b, err := os.ReadFile(p)
			if err == nil {
				return b, nil
			}
			if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrInvalid) && !isNotDir(err) {
				return nil, err
			}
		}
	}
	return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, name, key)
}

// GetString is Get with surrounding whitespace removed.
func (s *Store) GetString(name, key string) (string, error) {
	b, err := s.Get(name, key)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Lookup returns all known keys of a secret, reading only the given keys.
func (s *Store) Lookup(name string, keys ...string) map[string][]byte {
	out := map[string][]byte{}
	for _, k := range keys {
		if v, err := s.Get(name, k); err == nil {
			out[k] = v
		}
	}
	return out
}

func validPart(p string) error {
	if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "/\\") {
		return fmt.Errorf("invalid secret reference %q", p)
	}
	return nil
}

func isNotDir(err error) bool {
	var pe *os.PathError
	return errors.As(err, &pe) && strings.Contains(pe.Err.Error(), "not a directory")
}

// All returns every key of a secret directory <dir>/<name>/.
func (s *Store) All(name string) (map[string][]byte, error) {
	if err := validPart(name); err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for k, v := range s.Overlay[name] {
		out[k] = v
	}
	if s.Dir != "" {
		entries, err := os.ReadDir(filepath.Join(s.Dir, name))
		if err != nil && len(out) == 0 {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(s.Dir, name, e.Name()))
			if err != nil {
				return nil, err
			}
			out[e.Name()] = b
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return out, nil
}
