package beads

import (
	"context"
	"errors"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeCreatedWindowNarrowsDenseHistoryAndIncludesBoundaryTies(t *testing.T) {
	boundary := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	filter := beadslib.IssueFilter{Limit: 2, Labels: []string{"tracking"}, SortBy: "created"}
	searches := 0
	want := []*beadslib.Issue{{ID: "b", CreatedAt: boundary}, {ID: "a", CreatedAt: boundary}}
	got, err := nativeSearchCreatedWindow(context.Background(), 2, filter,
		func(_ context.Context, limit int) ([]time.Time, error) {
			if limit != 4 {
				t.Fatalf("candidate limit = %d, want 4", limit)
			}
			return []time.Time{boundary.Add(time.Second), boundary, boundary, boundary}, nil
		},
		func(_ context.Context, bounded beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			searches++
			if bounded.CreatedAfter == nil || !bounded.CreatedAfter.Before(boundary) {
				t.Fatal("search must use an indexed time range including every boundary tie")
			}
			if bounded.Limit != filter.Limit || bounded.Labels[0] != "tracking" {
				t.Fatalf("search lost original filters: %+v", bounded)
			}
			return want, nil
		})
	if err != nil || len(got) != 2 || searches != 1 {
		t.Fatalf("search = %v, %v, calls=%d", got, err, searches)
	}
	if filter.CreatedAfter != nil {
		t.Fatal("optimization mutated the caller's filter")
	}
}

func TestNativeCreatedWindowExactPageKeepsAllBoundaryTies(t *testing.T) {
	boundary := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	filter := beadslib.IssueFilter{Limit: 0, Labels: []string{"tracking"}, SortBy: "created"}
	issues, err := nativeSearchCreatedWindow(context.Background(), 2, filter,
		func(_ context.Context, limit int) ([]time.Time, error) {
			if limit != 4 {
				t.Fatalf("candidate limit = %d, want 4", limit)
			}
			return []time.Time{boundary, boundary, boundary, boundary}, nil
		},
		func(_ context.Context, bounded beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			if bounded.CreatedAfter == nil || !bounded.CreatedAfter.Before(boundary) {
				t.Fatal("exact page must use the indexed range including every boundary tie")
			}
			if bounded.Limit != 0 {
				t.Fatal("backing limit would drop larger-ID ties needed for exact client ordering")
			}
			var result []*beadslib.Issue
			for _, id := range []string{"a", "b", "c", "d"} {
				result = append(result, &beadslib.Issue{ID: id, CreatedAt: boundary, Labels: []string{"tracking"}})
			}
			return result, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	var rows []Bead
	for _, issue := range issues {
		rows = append(rows, Bead{ID: issue.ID, CreatedAt: issue.CreatedAt, Labels: issue.Labels})
	}
	page := ApplyListQuery(rows, ListQuery{Label: "tracking", Limit: 2, Sort: SortCreatedDesc, IncludeClosed: true, TierMode: TierBoth})
	if len(page) != 2 || page[0].ID != "d" || page[1].ID != "c" {
		t.Fatalf("exact page = %v, want d,c", page)
	}
}

func TestNativeCreatedWindowSparseHistoryFallsBackWithoutDroppingOldMatches(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	probes, searches := 0, 0
	want := []*beadslib.Issue{{ID: "old", CreatedAt: now.Add(-24 * time.Hour)}}
	got, err := nativeSearchCreatedWindow(context.Background(), 2, beadslib.IssueFilter{Limit: 2},
		func(_ context.Context, limit int) ([]time.Time, error) {
			probes++
			times := make([]time.Time, limit)
			for i := range times {
				times[i] = now.Add(-time.Duration(i) * time.Second)
			}
			return times, nil
		},
		func(_ context.Context, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			searches++
			if filter.CreatedAfter == nil {
				return want, nil
			}
			return nil, nil
		})
	if err != nil || len(got) != 1 || got[0].ID != "old" || probes != 3 || searches != 4 {
		t.Fatalf("sparse search = %v, %v, probes=%d searches=%d", got, err, probes, searches)
	}
}

func TestNativeCreatedWindowExhaustedHistoryDoesNotRepeatSearch(t *testing.T) {
	searches := 0
	_, err := nativeSearchCreatedWindow(context.Background(), 2, beadslib.IssueFilter{Limit: 2},
		func(context.Context, int) ([]time.Time, error) { return nil, nil },
		func(_ context.Context, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			searches++
			if filter.CreatedAfter != nil {
				t.Fatal("empty window must use the original filter")
			}
			return nil, nil
		})
	if err != nil || searches != 1 {
		t.Fatalf("empty search: %v, calls=%d", err, searches)
	}
}

func TestNativeCreatedWindowSmallCorpusKeepsFullSearchScope(t *testing.T) {
	searches := 0
	_, err := nativeSearchCreatedWindow(context.Background(), 2, beadslib.IssueFilter{Limit: 2},
		func(context.Context, int) ([]time.Time, error) {
			return []time.Time{time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}, nil
		},
		func(_ context.Context, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			searches++
			if filter.CreatedAfter != nil {
				t.Fatal("small corpus must keep the original scope, including backdated inserts after the probe")
			}
			return nil, nil
		})
	if err != nil || searches != 1 {
		t.Fatalf("small corpus search: %v, calls=%d", err, searches)
	}
}

func TestNativeCreatedWindowPropagatesProbeAndSearchFailures(t *testing.T) {
	wantErr := errors.New("read failed")
	for _, probeFails := range []bool{true, false} {
		t.Run(map[bool]string{true: "probe", false: "search"}[probeFails], func(t *testing.T) {
			searches := 0
			_, err := nativeSearchCreatedWindow(context.Background(), 2, beadslib.IssueFilter{Limit: 2},
				func(context.Context, int) ([]time.Time, error) {
					if probeFails {
						return nil, wantErr
					}
					return []time.Time{time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}, nil
				},
				func(context.Context, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
					searches++
					return nil, wantErr
				})
			if !errors.Is(err, wantErr) || (probeFails && searches != 0) || (!probeFails && searches != 1) {
				t.Fatalf("error=%v, searches=%d", err, searches)
			}
		})
	}
}

func TestNativeSummaryProjectionOmitsDetailsAndDependencies(t *testing.T) {
	filter := nativeIssueFilterFromListQuery(ListQuery{SkipDetails: true})
	if !filter.Lite || filter.IncludeDependencies {
		t.Fatalf("summary filter must omit text and dependencies: Lite=%v IncludeDependencies=%v", filter.Lite, filter.IncludeDependencies)
	}
	full := nativeIssueFilterFromListQuery(ListQuery{})
	if full.Lite || !full.IncludeDependencies {
		t.Fatal("complete-record queries must retain their projection")
	}
}

func TestNativeCreatedWindowPreservesStrongerCallerCutoff(t *testing.T) {
	cutoff := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	searches := 0
	_, err := nativeSearchCreatedWindow(context.Background(), 2, beadslib.IssueFilter{Limit: 2, CreatedAfter: &cutoff},
		func(context.Context, int) ([]time.Time, error) {
			return []time.Time{cutoff.Add(time.Second), cutoff, cutoff, cutoff.Add(-time.Second)}, nil
		},
		func(_ context.Context, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			searches++
			if !filter.CreatedAfter.Equal(cutoff) {
				t.Fatalf("caller cutoff was broadened to %s", filter.CreatedAfter)
			}
			return nil, nil
		})
	if err != nil || searches != 1 {
		t.Fatalf("caller-bounded search: %v, searches=%d", err, searches)
	}
}
