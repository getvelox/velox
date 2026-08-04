package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/sagarsuperuser/velox/internal/audit"
	"github.com/sagarsuperuser/velox/internal/config"
	"github.com/sagarsuperuser/velox/internal/platform/migrate"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
	"github.com/sagarsuperuser/velox/internal/tenant"
	"github.com/sagarsuperuser/velox/internal/user"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	cfg, err := config.Load()
	if err != nil {
		fatal("load config: %v", err)
	}

	// Run migrations first — migrate.Up manages its own short-lived pool.
	if err := migrate.Up(cfg.DB.URL); err != nil {
		fatal("migrations: %v", err)
	}

	pool, err := config.OpenPostgres(cfg.DB)
	if err != nil {
		fatal("open database: %v", err)
	}
	defer func() { _ = pool.Close() }()

	db := postgres.NewDB(pool, 5*time.Second)
	ctx := context.Background()

	bootstrapEmail := strings.TrimSpace(os.Getenv("VELOX_BOOTSTRAP_EMAIL"))
	bootstrapPassword := strings.TrimSpace(os.Getenv("VELOX_BOOTSTRAP_PASSWORD"))

	// Tenant name resolution order:
	//   1. VELOX_BOOTSTRAP_TENANT env var — works through `make bootstrap`
	//      (make swallows positional args and re-interprets them as
	//      separate targets, so `make bootstrap "Tenant B"` would NOT
	//      forward "Tenant B" to the binary)
	//   2. Positional arg(s) — works when invoking the binary directly
	//      (`go run ./cmd/velox-bootstrap "Tenant B"`)
	//   3. RunBootstrap's default: "Demo Tenant"
	tenantName := strings.TrimSpace(os.Getenv("VELOX_BOOTSTRAP_TENANT"))
	if tenantName == "" && len(os.Args) > 1 {
		tenantName = strings.Join(os.Args[1:], " ")
	}

	// The CLI is a thin caller of the single bootstrap writer
	// (tenant.RunBootstrap, ADR-073): one all-or-nothing tx creates
	// tenant + settings + keys + owner user, guards authoritative under
	// the bootstrap advisory lock. The CLI does NOT block
	// additional-tenant creation — Velox's model is multi-tenant in the
	// data layer; pass a different email to spin up a second tenant in
	// the same deployment (useful for cross-tenant tests, FLOW X1 /
	// A2-disagreeing-identities, and ahead of any "platform admin" UI).
	userStore := user.NewPostgresStore(db)
	result, err := tenant.RunBootstrap(ctx, db, tenant.BootstrapDeps{
		HashPassword: user.HashPassword,
		CreateUserTx: userStore.CreateInTx,
		// Same provisioning rows as the HTTP bootstrap route (ADR-090): the
		// CLI path must not be the one that mints a live key unrecorded.
		Audit: audit.NewLogger(db),
	}, tenant.BootstrapOpts{
		TenantName:      tenantName,
		OwnerEmail:      bootstrapEmail,
		OwnerPassword:   bootstrapPassword,
		FirstTenantOnly: false,
	})
	if err != nil {
		if errors.Is(err, tenant.ErrOwnerEmailExists) {
			printEmailExistsGuidance(bootstrapEmail)
		}
		fatal("bootstrap: %v", err)
	}

	fmt.Println("========================================")
	fmt.Println("  Velox Bootstrap Complete")
	fmt.Println("========================================")
	fmt.Println()
	fmt.Printf("  Tenant:     %s\n", result.Tenant.Name)
	fmt.Printf("  Tenant ID:  %s\n", result.Tenant.ID)
	fmt.Println()
	fmt.Println("  Dashboard sign-in (http://localhost:5173/login):")
	fmt.Printf("  Email:    %s\n", result.OwnerUser.Email)
	if result.PasswordGenerated {
		fmt.Printf("  Password: %s   (generated — save this; not retrievable)\n", result.OwnerPassword)
	} else {
		fmt.Println("  Password: (the value of VELOX_BOOTSTRAP_PASSWORD)")
	}
	fmt.Println()
	fmt.Println("  API keys for SDK / curl callers:")
	fmt.Println()
	fmt.Println("  Secret Key — TEST mode (no real money):")
	fmt.Printf("  %s\n", result.TestSecretKey)
	fmt.Println()
	fmt.Println("  Secret Key — LIVE mode (charges real cards):")
	fmt.Printf("  %s\n", result.LiveSecretKey)
	fmt.Println()
	fmt.Println("  Publishable Key (restricted, test mode):")
	fmt.Printf("  %s\n", result.TestPublishableKey)
	fmt.Println()
	fmt.Println("  Try it on the API:")
	fmt.Printf("  curl -H 'Authorization: Bearer %s' http://localhost:8080/v1/customers\n", result.TestSecretKey)
	fmt.Println()
	fmt.Println("  Need a second tenant for cross-tenant tests? Re-run with a")
	fmt.Println("  different email:")
	fmt.Println(`    make bootstrap VELOX_BOOTSTRAP_EMAIL=tenant-b@local \`)
	fmt.Println(`      VELOX_BOOTSTRAP_PASSWORD='choose-a-password' \`)
	fmt.Println(`      VELOX_BOOTSTRAP_TENANT='Tenant B'`)
	fmt.Println("========================================")
}

// suggestFreeEmail returns an example owner address that is guaranteed to
// differ from the one that just collided, so the printed remedy is always
// runnable as-is.
func suggestFreeEmail(taken string) string {
	const preferred = "tenant-b@local"
	if !strings.EqualFold(strings.TrimSpace(taken), preferred) {
		return preferred
	}
	return "tenant-c@local"
}

// printEmailExistsGuidance keeps the CLI's actionable re-run options on
// the owner-email conflict — the one bootstrap failure a dev hits
// routinely (re-running `make bootstrap` with the default email).
func printEmailExistsGuidance(email string) {
	if email == "" {
		email = "admin@velox.local"
	}
	fmt.Fprintf(os.Stderr, "ERROR: an account already exists for %s.\n\n", email)
	fmt.Fprintln(os.Stderr, "Velox is already bootstrapped for this email. Pick one:")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  1. Sign in with the existing credentials at http://localhost:5173/login")
	fmt.Fprintln(os.Stderr, "  2. Create an ADDITIONAL tenant in the same deployment — each tenant")
	fmt.Fprintln(os.Stderr, "     needs its OWN owner email, so pick one you have not used yet:")
	fmt.Fprintln(os.Stderr, "")
	// Suggest an address that differs from the one that just failed. The
	// example used to be a hardcoded tenant-b@local, so whenever THAT was
	// the conflicting email the remedy told the operator to re-run the
	// exact command that had just errored.
	fmt.Fprintf(os.Stderr, "       make bootstrap VELOX_BOOTSTRAP_EMAIL=%s \\\n", suggestFreeEmail(email))
	fmt.Fprintln(os.Stderr, `         VELOX_BOOTSTRAP_PASSWORD='choose-a-password' \`)
	fmt.Fprintln(os.Stderr, `         VELOX_BOOTSTRAP_TENANT='Tenant B'`)
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  3. Wipe and re-bootstrap (loses all dev data):")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "       docker compose down -v && docker compose up -d postgres redis mailpit")
	fmt.Fprintln(os.Stderr, "       make bootstrap")
	os.Exit(1)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+format+"\n", args...)
	os.Exit(1)
}
