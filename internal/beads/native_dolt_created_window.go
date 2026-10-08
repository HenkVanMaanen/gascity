package beads

import (
	"context"
	"database/sql"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func nativeSearchCreatedWindow(ctx context.Context, filter beadslib.IssueFilter,
	recent func(context.Context, int) ([]time.Time, error),
	search func(context.Context, beadslib.IssueFilter) ([]*beadslib.Issue, error),
) ([]*beadslib.Issue, error) {
	if filter.Limit <= 0 || filter.Limit > 4096 {
		return search(ctx, filter)
	}
	window := 2 * filter.Limit
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
		if len(issues) >= filter.Limit ||
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
	if !ok || accessor.DB() == nil || query.Sort != SortCreatedDesc ||
		!query.AllowBackingCreatedLimit || query.Label == "" || filter.Limit < 2 {
		return search(ctx, filter)
	}
	return nativeSearchCreatedWindow(ctx, filter,
		func(ctx context.Context, limit int) ([]time.Time, error) {
			return nativeRecentCreatedTimes(ctx, accessor.DB(), limit)
		}, search)
}
