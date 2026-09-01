// Package agentstore persists the set of monitored agents - each one's
// poll address and an optional user-assigned tag - to a JSON file on disk.
// This is what lets an agent added or tagged from the dashboard survive a
// backend restart and outlive any single login session, without pulling in
// a database.
package agentstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Agent is one monitored host: its poll address and an optional
// user-assigned label shown on the dashboard in place of the bare address.
type Agent struct {
	Address string `json:"address"`
	Tag     string `json:"tag,omitempty"`
}

// maxTagLength caps a tag's stored length so a runaway client can't bloat
// the config file or blow out the dashboard card layout.
const maxTagLength = 60

var (
	ErrNotFound = errors.New("agent not found")
	ErrExists   = errors.New("agent already exists")
)

// Store is a small file-backed registry of monitored agents. Every mutation
// is persisted to disk immediately, atomically (temp file + rename), so the
// process can crash mid-write without corrupting the file or silently
// losing an edit.
type Store struct {
	path string

	mu     sync.Mutex
	agents []Agent
}

// Load reads path into a Store. If path doesn't exist yet (first run), the
// store is seeded from defaults - deduplicated, in order - and immediately
// persisted, so a fresh install still has something to poll and a real file
// on disk to edit from then on.
func Load(path string, defaults []string) (*Store, error) {
	s := &Store{path: path}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		seen := make(map[string]bool, len(defaults))
		for _, addr := range defaults {
			if seen[addr] {
				continue
			}
			seen[addr] = true
			s.agents = append(s.agents, Agent{Address: addr})
		}
		if err := s.persist(); err != nil {
			return nil, fmt.Errorf("seed agent store: %w", err)
		}
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read agent store: %w", err)
	}

	if err := json.Unmarshal(data, &s.agents); err != nil {
		return nil, fmt.Errorf("parse agent store %s: %w", path, err)
	}
	return s, nil
}

// List returns a snapshot of every configured agent, in the stable order
// they were added. Never nil, so callers that json-encode it directly get
// [] rather than null when no agents are configured.
func (s *Store) List() []Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]Agent, len(s.agents))
	copy(list, s.agents)
	return list
}

// Addresses returns just the addresses, in the same order as List - this is
// what pollLoop feeds to pollAll each cycle.
func (s *Store) Addresses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	addrs := make([]string, len(s.agents))
	for i, a := range s.agents {
		addrs[i] = a.Address
	}
	return addrs
}

// Tag returns the current tag for address, or "" if it has none or isn't
// configured.
func (s *Store) Tag(address string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.agents {
		if a.Address == address {
			return a.Tag
		}
	}
	return ""
}

// Has reports whether address is currently configured.
func (s *Store) Has(address string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.agents {
		if a.Address == address {
			return true
		}
	}
	return false
}

// Add registers a new agent. It rejects a malformed address (must be
// host:port, matching what poll() assumes), an empty address, and a
// duplicate of one already configured.
func (s *Store) Add(address, tag string) (Agent, error) {
	address = strings.TrimSpace(address)
	tag = normalizeTag(tag)

	if err := validateAddress(address); err != nil {
		return Agent{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, a := range s.agents {
		if a.Address == address {
			return Agent{}, ErrExists
		}
	}

	prev := append([]Agent(nil), s.agents...)
	agent := Agent{Address: address, Tag: tag}
	s.agents = append(s.agents, agent)
	if err := s.persist(); err != nil {
		s.agents = prev
		return Agent{}, err
	}
	return agent, nil
}

// SetTag updates the tag on an already-configured agent.
func (s *Store) SetTag(address, tag string) error {
	tag = normalizeTag(tag)

	s.mu.Lock()
	defer s.mu.Unlock()

	for i, a := range s.agents {
		if a.Address == address {
			prevTag := a.Tag
			s.agents[i].Tag = tag
			if err := s.persist(); err != nil {
				s.agents[i].Tag = prevTag
				return err
			}
			return nil
		}
	}
	return ErrNotFound
}

// Remove deregisters an agent so it's no longer polled.
func (s *Store) Remove(address string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	idx := -1
	for i, a := range s.agents {
		if a.Address == address {
			idx = i
			break
		}
	}
	if idx == -1 {
		return ErrNotFound
	}

	prev := append([]Agent(nil), s.agents...)
	s.agents = append(s.agents[:idx], s.agents[idx+1:]...)
	if err := s.persist(); err != nil {
		s.agents = prev
		return err
	}
	return nil
}

// persist writes the current agent list to disk atomically: write to a temp
// file in the same directory, then rename over the real path, so a crash
// mid-write can't leave a truncated or corrupt config file. Caller must
// hold mu.
func (s *Store) persist() error {
	data, err := json.MarshalIndent(s.agents, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".agents-*.json.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, s.path)
}

// normalizeTag trims a tag and caps its length. The cap counts runes, not
// bytes: slicing a string at a byte offset can cut a multi-byte character
// in half, leaving invalid UTF-8 that json.Marshal silently rewrites to
// U+FFFD - i.e. a tag with an emoji or an accent could come back corrupted.
func normalizeTag(tag string) string {
	tag = strings.TrimSpace(tag)
	if utf8.RuneCountInString(tag) > maxTagLength {
		tag = string([]rune(tag)[:maxTagLength])
	}
	return tag
}

// validateAddress requires a non-empty host:port pair with a numeric port -
// the same shape poll() assumes when it does "http://" + address + "/stats".
//
// net.SplitHostPort alone is not enough: it is purely syntactic and happily
// accepts "host:80/admin?x=y" as port "80/admin?x=y", which would smuggle a
// path and query into the URL the backend builds. Requiring a real port
// number keeps an address to exactly the host and port it claims to be.
func validateAddress(address string) error {
	if address == "" {
		return fmt.Errorf("address is required")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("address must be host:port: %w", err)
	}
	if host == "" || port == "" {
		return fmt.Errorf("address must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port must be a number between 1 and 65535, got %q", port)
	}
	return nil
}
