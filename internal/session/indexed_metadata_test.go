package session

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type indexedMetadataStore struct {
	*beads.MemStore
}

func (s *indexedMetadataStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if len(query.Metadata) > 0 && query.Type != BeadType && query.Label != LabelSession {
		return nil, fmt.Errorf("metadata lookup would scan unrelated history: %+v", query)
	}
	return s.MemStore.List(query)
}

func TestSessionMetadataLookupsUseIndexedUnion(t *testing.T) {
	lookups := []struct {
		name          string
		includeClosed bool
		lookup        func(beads.Store) ([]beads.Bead, error)
	}{
		{"live resolver", false, func(s beads.Store) ([]beads.Bead, error) {
			return listSessionBeadsByMetadata(s, "alias", "target", false)
		}},
		{"closed resolver", true, func(s beads.Store) ([]beads.Bead, error) {
			return listSessionBeadsByMetadata(s, "alias", "target", true)
		}},
		{"configured named lookup", false, func(s beads.Store) ([]beads.Bead, error) {
			return listConfiguredNamedSessionBeadsByMetadata(s, "alias", "target")
		}},
		{"metadata candidates", true, func(s beads.Store) ([]beads.Bead, error) {
			return ExactMetadataSessionCandidates(s, true, map[string]string{"alias": "target"})
		}},
		{"metadata candidates with status", true, func(s beads.Store) ([]beads.Bead, error) {
			return ExactMetadataSessionCandidatesWithStatus(s, "closed", map[string]string{"alias": "target"})
		}},
	}
	for _, tc := range lookups {
		t.Run(tc.name, func(t *testing.T) {
			store := &indexedMetadataStore{MemStore: beads.NewMemStore()}
			want := make(map[string]bool)
			for _, shape := range []struct {
				typ     string
				labeled bool
				closed  bool
				matches bool
			}{
				{BeadType, false, false, true}, // type-only sessions remain visible
				{BeadType, true, false, true},  // union must deduplicate
				{"", true, false, true},        // repairable sessions remain visible
				{"task", true, false, false},   // marker alone cannot turn tasks into sessions
				{"", false, false, false},
				{BeadType, true, true, true},
			} {
				var labels []string
				if shape.labeled {
					labels = []string{LabelSession}
				}
				b, err := store.Create(beads.Bead{Type: BeadType, Labels: labels, Metadata: map[string]string{"alias": "target"}})
				if err != nil {
					t.Fatal(err)
				}
				opts := beads.UpdateOpts{Type: &shape.typ}
				if err := store.Update(b.ID, opts); err != nil {
					t.Fatal(err)
				}
				if shape.closed {
					if err := store.Close(b.ID); err != nil {
						t.Fatal(err)
					}
				}
				if shape.matches && (!shape.closed || tc.includeClosed) && (tc.name != "metadata candidates with status" || shape.closed) {
					want[b.ID] = true
				}
			}
			got, err := tc.lookup(store)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("lookup returned %d records, want %d: %+v", len(got), len(want), got)
			}
			for _, record := range got {
				if !want[record.ID] || record.Type != BeadType {
					t.Fatalf("unexpected or unnormalized session: %+v", record)
				}
				delete(want, record.ID)
			}
		})
	}
}
