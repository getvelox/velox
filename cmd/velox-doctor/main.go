// velox-doctor sweeps a Velox database for money-invariant violations —
// states no legal writer can produce. Read-only; exits 1 if any violation
// is found, 2 if any check failed to run.
//
//	DATABASE_URL="postgres://…" go run ./cmd/velox-doctor
//
// DATABASE_URL must be a role that sees every row — the table owner or an
// admin. An RLS-scoped app role (velox_app) sees NOTHING and the doctor
// would report a spotless bill of health over rows it cannot see.
//
// See internal/doctor for the invariant catalog and the exactness rule, and
// docs/dev/product-stability-plan.md for why this exists.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/doctor"
)

func main() {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "velox-doctor: DATABASE_URL is required")
		os.Exit(2)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "velox-doctor: open: %v\n", err)
		os.Exit(2)
	}
	defer func() { _ = db.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res := doctor.Run(ctx, db, doctor.Checks)

	fmt.Printf("velox-doctor: %d checks in %s\n", res.Checks, res.Duration.Round(time.Millisecond))
	for _, e := range res.Errors {
		fmt.Printf("  CHECK-ERROR %v\n", e)
	}
	if len(res.Violations) == 0 && len(res.Errors) == 0 {
		fmt.Println("  OK — no invariant violations")
		return
	}
	byCheck := map[string]int{}
	for _, v := range res.Violations {
		byCheck[v.Check.Name]++
		fmt.Printf("  VIOLATION [%s/%s] %s  %s\n      why: %s\n",
			v.Check.Domain, v.Check.Name, v.RowID, v.Detail, v.Check.Why)
	}
	fmt.Printf("velox-doctor: %d violation(s) across %d check(s)\n", len(res.Violations), len(byCheck))
	if len(res.Errors) > 0 {
		os.Exit(2)
	}
	os.Exit(1)
}
