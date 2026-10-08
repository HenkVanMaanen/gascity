package beads

import (
	"context"
	"database/sql"
	"sort"

	beadslib "github.com/steveyegge/beads"
)

func nativeCanReadClosedSummary(query ListQuery) bool {
	return query.SkipDetails && !query.SkipLabels && query.Status == "closed" &&
		(query.Sort == SortCreatedDesc || query.Sort == SortCreatedAsc) &&
		query.Label != "" && query.Limit == 0 && query.TierMode == TierBoth &&
		query.Type == "" && query.Assignee == "" && len(query.Assignees) == 0 &&
		query.ParentID == "" && len(query.ParentIDs) == 0 && len(query.Metadata) == 0 &&
		query.CreatedBefore.IsZero() && query.UpdatedBefore.IsZero() && query.SeekAfter == nil
}

// nativeReadClosedSummary fetches narrow records and their labels in one query
// per storage plane. It avoids DISTINCT over wide records, lease joins, and the
// enormous IN-list hydration query generated for retained order history.
func nativeReadClosedSummary(ctx context.Context, db *sql.DB, label string) ([]*beadslib.Issue, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var issues []*beadslib.Issue
	seen := make(map[string]bool)
	for _, plane := range [][2]string{{"issues", "labels"}, {"wisps", "wisp_labels"}} {
		// Keep the same corrupt-metadata exclusion as NativeDoltStore.List.
		// NULL and JSON null decode to an empty map; non-object values fail.
		query := `SELECT m.id, m.title, m.status, m.created_at, m.updated_at,
			m.ephemeral, m.no_history,
			(m.metadata IS NULL OR JSON_TYPE(m.metadata) IN ('OBJECT', 'NULL')),
			all_labels.label
			FROM ` + plane[0] + ` m
			JOIN ` + plane[1] + ` selected ON selected.issue_id = m.id AND selected.label = ?
			JOIN ` + plane[1] + ` all_labels ON all_labels.issue_id = m.id
			WHERE m.status = 'closed'`
		rows, err := tx.QueryContext(ctx, query, label)
		if err != nil {
			if isTableNotExistError(err) {
				return nil, false, nil // Upstream owns legacy-schema routing.
			}
			return nil, false, err
		}
		byID := make(map[string]*beadslib.Issue)
		for rows.Next() {
			issue := &beadslib.Issue{Priority: 2}
			var rowLabel string
			var validMetadata bool
			if err := rows.Scan(&issue.ID, &issue.Title, &issue.Status, &issue.CreatedAt,
				&issue.UpdatedAt, &issue.Ephemeral, &issue.NoHistory, &validMetadata, &rowLabel); err != nil {
				_ = rows.Close()
				return nil, false, err
			}
			existing := byID[issue.ID]
			if existing == nil {
				if seen[issue.ID] {
					_ = rows.Close()
					// Preserve upstream's overlapping-plane error rather than merge
					// conflicting records into a summary.
					return nil, false, nil
				}
				seen[issue.ID] = true
				byID[issue.ID] = issue
				if validMetadata {
					issues = append(issues, issue)
				}
				existing = issue
			}
			existing.Labels = append(existing.Labels, rowLabel)
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, false, err
		}
		if closeErr != nil {
			return nil, false, closeErr
		}
	}
	for _, issue := range issues {
		sort.Strings(issue.Labels)
	}
	return issues, true, nil
}
