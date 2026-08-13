package analytics

import (
	"log/slog"
	"net/http"

	"github.com/shopspring/decimal"

	"github.com/sagarsuperuser/velox/internal/api/respond"
	"github.com/sagarsuperuser/velox/internal/auth"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

// UsagePoint is one bucket of aggregated usage events.
//
// Quantity is decimal, not int64: usage_events.quantity is NUMERIC(38,12)
// (ADR-005/ADR-045), so SUM() comes back as a decimal string. Scanning it into
// an int64 made every one of these three queries fail at the driver — this
// endpoint returned 500 for any tenant that had ingested a single event. It
// wire-encodes as a string for the same reason Aggregate.TotalUnits does:
// truncating to an integer would silently lose fractional usage.
type UsagePoint struct {
	Date     string          `json:"date"`
	Events   int64           `json:"events"`
	Quantity decimal.Decimal `json:"quantity"`
}

// TopMeter is one row of the top-N usage-by-meter breakdown.
type TopMeter struct {
	MeterID   string          `json:"meter_id"`
	MeterName string          `json:"meter_name"`
	Key       string          `json:"key"`
	Events    int64           `json:"events"`
	Quantity  decimal.Decimal `json:"quantity"`
}

// usageAnalytics returns usage-event counts over time and top-N meters by
// event volume for the requested period.
func (h *Handler) usageAnalytics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := auth.TenantID(ctx)
	period := parsePeriod(r.URL.Query().Get("period"))

	tx, err := h.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		slog.Error("analytics usage: begin tx", "error", err)
		respond.InternalError(w, r)
		return
	}
	defer func() { _ = tx.Rollback() }()

	dateFmt := "YYYY-MM-DD"
	if period.Trunc == "month" {
		dateFmt = "YYYY-MM"
	}

	// Timeseries.
	//
	// Every usage_events query below names tenant_id explicitly even though
	// the tx is RLS-scoped. The RLS policy is an OR (bypass_rls OR tenant
	// match), so Postgres applies it as a post-scan Filter and never as an
	// Index Cond — and every usage_events btree leads with tenant_id. Without
	// the predicate these three scans read every tenant's events.
	rows, err := tx.QueryContext(ctx, `
		SELECT to_char(date_trunc($1, timestamp), $2) AS d,
		       COUNT(*) AS events,
		       COALESCE(SUM(quantity), 0) AS qty
		FROM usage_events
		WHERE tenant_id = $5 AND timestamp >= $3 AND timestamp < $4 AND customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NULL)
		GROUP BY 1
		ORDER BY 1
	`, period.Trunc, dateFmt, period.Start, period.End, tenantID)
	if err != nil {
		slog.Error("analytics usage: timeseries query", "error", err)
		respond.InternalError(w, r)
		return
	}
	series := make([]UsagePoint, 0)
	for rows.Next() {
		var p UsagePoint
		if err := rows.Scan(&p.Date, &p.Events, &p.Quantity); err != nil {
			_ = rows.Close()
			slog.Error("analytics usage: timeseries scan", "error", err)
			respond.InternalError(w, r)
			return
		}
		series = append(series, p)
	}
	_ = rows.Close()

	// Top meters by event volume (cap at 10)
	rows, err = tx.QueryContext(ctx, `
		SELECT m.id, m.name, m.key,
		       COUNT(ue.id) AS events,
		       COALESCE(SUM(ue.quantity), 0) AS qty
		FROM meters m
		LEFT JOIN usage_events ue
		  ON ue.tenant_id = $3 AND ue.meter_id = m.id
		 AND ue.timestamp >= $1 AND ue.timestamp < $2
		 AND ue.customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NULL)
		GROUP BY m.id, m.name, m.key
		HAVING COUNT(ue.id) > 0
		ORDER BY events DESC
		LIMIT 10
	`, period.Start, period.End, tenantID)
	if err != nil {
		slog.Error("analytics usage: top-meters query", "error", err)
		respond.InternalError(w, r)
		return
	}
	top := make([]TopMeter, 0)
	for rows.Next() {
		var t TopMeter
		if err := rows.Scan(&t.MeterID, &t.MeterName, &t.Key, &t.Events, &t.Quantity); err != nil {
			_ = rows.Close()
			slog.Error("analytics usage: top-meters scan", "error", err)
			respond.InternalError(w, r)
			return
		}
		top = append(top, t)
	}
	_ = rows.Close()

	// Totals
	var totalEvents int64
	var totalQuantity decimal.Decimal
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(quantity), 0)
		FROM usage_events
		WHERE tenant_id = $3 AND timestamp >= $1 AND timestamp < $2 AND customer_id IN (SELECT id FROM customers WHERE test_clock_id IS NULL)
	`, period.Start, period.End, tenantID).Scan(&totalEvents, &totalQuantity); err != nil {
		slog.Error("analytics usage: totals", "error", err)
		respond.InternalError(w, r)
		return
	}

	_ = tx.Commit()
	respond.JSON(w, r, http.StatusOK, map[string]any{
		"period":     period.Key,
		"data":       series,
		"top_meters": top,
		"totals": map[string]any{
			"events":   totalEvents,
			"quantity": totalQuantity,
		},
	})
}
