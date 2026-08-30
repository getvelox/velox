package leader

import (
	"context"
	"database/sql"
	"fmt"
)

// StatusRow is one role's row from the leader_status view.
type StatusRow struct {
	Role         Role
	Held         bool
	HolderID     string
	HasTicked    bool
	LastTickAgeS float64
	Paused       bool
	PauseReason  string
}

// Status reads the leader_status view (all roles). It is the cluster fact
// behind the velox_leader_* gauges and the runbook's first query; a role
// that has never ticked reports HasTicked=false with LastTickAgeS=0.
func Status(ctx context.Context, db *sql.DB) ([]StatusRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT role, held, COALESCE(holder_id, ''), last_tick_age_s,
		       paused_at IS NOT NULL, COALESCE(pause_reason, '')
		  FROM leader_status
		 ORDER BY role`)
	if err != nil {
		return nil, fmt.Errorf("leader status: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []StatusRow
	for rows.Next() {
		var r StatusRow
		var age sql.NullFloat64
		var role string
		if err := rows.Scan(&role, &r.Held, &r.HolderID, &age, &r.Paused, &r.PauseReason); err != nil {
			return nil, fmt.Errorf("leader status: scan: %w", err)
		}
		r.Role = Role(role)
		r.HasTicked = age.Valid
		r.LastTickAgeS = age.Float64
		out = append(out, r)
	}
	return out, rows.Err()
}
