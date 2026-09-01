package agentstore

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLoadSeedsFromDefaultsOnFirstRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")

	s, err := Load(path, []string{"a:1", "b:2", "a:1"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got := s.Addresses()
	want := []string{"a:1", "b:2"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Addresses = %v, want %v (deduplicated)", got, want)
	}

	// A second Load from the same path should read back what was seeded,
	// not re-seed from defaults.
	s2, err := Load(path, []string{"different:9"})
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if got := s2.Addresses(); len(got) != 2 {
		t.Fatalf("second Load Addresses = %v, want the persisted seed, not defaults", got)
	}
}

func TestAddPersistsAcrossLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")

	s, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if _, err := s.Add("192.168.1.10:8080", "Plex server"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	reloaded, err := Load(path, nil)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	list := reloaded.List()
	if len(list) != 1 || list[0].Address != "192.168.1.10:8080" || list[0].Tag != "Plex server" {
		t.Fatalf("reloaded list = %+v, want the agent added before reload", list)
	}
}

func TestAddRejectsInvalidAndDuplicateAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	s, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	cases := []string{"", "no-port", "   "}
	for _, addr := range cases {
		if _, err := s.Add(addr, ""); err == nil {
			t.Errorf("Add(%q) should have failed validation", addr)
		}
	}

	if _, err := s.Add("host:1234", ""); err != nil {
		t.Fatalf("Add valid address: %v", err)
	}
	if _, err := s.Add("host:1234", "different tag"); !errors.Is(err, ErrExists) {
		t.Fatalf("Add duplicate = %v, want ErrExists", err)
	}
}

func TestSetTagUpdatesExistingAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	s, err := Load(path, []string{"host:1"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := s.SetTag("host:1", "  NAS  "); err != nil {
		t.Fatalf("SetTag: %v", err)
	}
	if got := s.Tag("host:1"); got != "NAS" {
		t.Errorf("Tag = %q, want trimmed %q", got, "NAS")
	}

	if err := s.SetTag("missing:1", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetTag on missing address = %v, want ErrNotFound", err)
	}
}

func TestSetTagTruncatesOverlongTag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	s, err := Load(path, []string{"host:1"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	long := ""
	for i := 0; i < maxTagLength+20; i++ {
		long += "x"
	}
	if err := s.SetTag("host:1", long); err != nil {
		t.Fatalf("SetTag: %v", err)
	}
	if got := s.Tag("host:1"); len(got) != maxTagLength {
		t.Errorf("tag length = %d, want %d", len(got), maxTagLength)
	}
}

func TestRemoveDeregistersAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agents.json")
	s, err := Load(path, []string{"a:1", "b:2"})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := s.Remove("a:1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if s.Has("a:1") {
		t.Error("a:1 should no longer be configured after Remove")
	}
	if got := s.Addresses(); len(got) != 1 || got[0] != "b:2" {
		t.Fatalf("Addresses after remove = %v, want [b:2]", got)
	}

	if err := s.Remove("a:1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove already-removed = %v, want ErrNotFound", err)
	}
}
