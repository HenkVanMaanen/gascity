package beads

import (
	"context"
	"database/sql"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func nativeSearchCreatedWindow(ctx context.Context, limit int, filter beadslib.IssueFilter,
	recent func(context.Context, int) ([]time.Time, error),
	search func(context.Context, beadslib.IssueFilter) ([]*beadslib.Issue, error),
) ([]*beadslib.Issue, error) {
	if limit <= 0 || limit > 4096 {
		return search(ctx, filter)
	}
	window := 2 * limit
	for range 3 {
		times, err := recent(ctx, window)
		if err != nil {
			return nil, err
		}
		if len(times) < window || times[len(times)-1].IsZero() {
			// A small corpus needs no range optimization. Keep the original scope
			// so backdated records inserted after the probe are still visible.
			return search(ctx, filter)
		}
		// Include the complete boundary second. Upstream's CreatedAfter is
		// exclusive and SQL timestamps can have less precision than Go times.
		// The label join and sort now see only this indexed recent range.
		cutoff := times[len(times)-1].Add(-time.Second)
		bounded := filter
		if bounded.CreatedAfter == nil || bounded.CreatedAfter.Before(cutoff) {
			bounded.CreatedAfter = &cutoff
		}
		issues, err := search(ctx, bounded)
		if err != nil {
			return nil, err
		}
		if len(issues) >= limit ||
			(filter.CreatedAfter != nil && !filter.CreatedAfter.Before(cutoff)) {
			return issues, nil
		}
		window *= 4
	}
	// Sparse labels or filters can put the newest match outside every window.
	// Preserve the original result rather than return an incomplete page.
	return search(ctx, filter)
}

// nativeRecentCreatedTimes reads a bounded prefix of each created_at index.
// The outer merge sorts at most twice limit timestamps, never the full corpus.
func nativeRecentCreatedTimes(ctx context.Context, db *sql.DB, limit int) ([]time.Time, error) {
	const query = `SELECT created_at FROM (
		(SELECT created_at FROM issues ORDER BY created_at DESC LIMIT ?)
		UNION ALL
		(SELECT created_at FROM wisps ORDER BY created_at DESC LIMIT ?)
	) AS recent ORDER BY created_at DESC LIMIT ?`
	rows, err := db.QueryContext(ctx, query, limit, limit, limit)
	if err != nil {
		if isTableNotExistError(err) {
			// Older schemas may lack wisps. The upstream search owns their routing.
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var times []time.Time
	for rows.Next() {
		var created time.Time
		if err := rows.Scan(&created); err != nil {
			return nil, err
		}
		times = append(times, created)
	}
	return times, rows.Err()
}

func nativeSearchListIssues(ctx context.Context, storage beadslib.Storage, query ListQuery, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	search := func(ctx context.Context, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
		return storage.SearchIssues(ctx, "", filter)
	}
	accessor, ok := storage.(rawDBGetter)
	if ok && accessor.DB() != nil && nativeCanReadClosedSummary(query) {
		issues, handled, err := nativeReadClosedSummary(ctx, accessor.DB(), query.Label)
		if handled || err != nil {
			return issues, err
		}
	}
	// A created-time range includes every boundary tie, even for exact reads
	// whose SQL limit must remain zero so ApplyListQuery can order IDs itself.
	// Reuse the pushdown eligibility checks to exclude client-only filters.
	eligible := query
	eligible.AllowBackingCreatedLimit = true
	limit := nativeCreatedLimitPushdown(eligible)
	if !ok || accessor.DB() == nil || query.Sort != SortCreatedDesc ||
		query.Label == "" || len(query.ParentIDs) > 0 || limit <= 0 {
		return search(ctx, filter)
	}
	searchMatching := func(ctx context.Context, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
		issues, err := search(ctx, filter)
		if err != nil {
			return nil, err
		}
		// Count only rows List can return. Corrupt metadata or residual filters
		// must not prematurely satisfy the window and hide older valid matches.
		matches := make([]*beadslib.Issue, 0, len(issues))
		for _, issue := range issues {
			bead, err := beadFromNativeIssue(issue)
			if isNativeIssueMetadataParseError(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if query.Matches(bead) {
				matches = append(matches, issue)
			}
		}
		return matches, nil
	}
	return nativeSearchCreatedWindow(ctx, limit, filter,
		func(ctx context.Context, limit int) ([]time.Time, error) {
			return nativeRecentCreatedTimes(ctx, accessor.DB(), limit)
		}, searchMatching)
}
