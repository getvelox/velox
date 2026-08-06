package customer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sagarsuperuser/velox/internal/domain"
	"github.com/sagarsuperuser/velox/internal/errs"
	"github.com/sagarsuperuser/velox/internal/platform/clock"
	"github.com/sagarsuperuser/velox/internal/platform/crypto"
	"github.com/sagarsuperuser/velox/internal/platform/postgres"
)

type PostgresStore struct {
	db      *postgres.DB
	enc     *crypto.Encryptor
	blinder *crypto.Blinder
}

func NewPostgresStore(db *postgres.DB) *PostgresStore {
	return &PostgresStore{db: db}
}

// SetEncryptor configures PII field encryption for customer data at rest.
// When set, email, display_name (Customer) and legal_name, email, phone,
// tax_identifier, tax_id (BillingProfile) are encrypted before write and
// decrypted after read.
func (s *PostgresStore) SetEncryptor(enc *crypto.Encryptor) {
	s.enc = enc
}

// SetBlinder configures the HMAC blinder used to populate customers.email_bidx.
// Required for the customer-initiated magic-link flow, optional otherwise —
// when unset, email_bidx is left NULL and FindByEmailBlindIndex returns no
// matches.
func (s *PostgresStore) SetBlinder(b *crypto.Blinder) {
	s.blinder = b
}

// emailBlindIndex normalises the email (trim + lowercase) and returns its
// blind-index representation. Empty string when the blinder isn't configured
// or the email is empty — both safe-by-default for INSERT/UPDATE.
func (s *PostgresStore) emailBlindIndex(email string) string {
	if s.blinder == nil {
		return ""
	}
	return s.blinder.Blind(strings.ToLower(strings.TrimSpace(email)))
}

// encryptCustomer encrypts PII fields on a Customer before writing to the DB.
func (s *PostgresStore) encryptCustomer(c domain.Customer) (domain.Customer, error) {
	if s.enc == nil {
		return c, nil
	}
	var err error
	if c.Email, err = s.enc.Encrypt(c.Email); err != nil {
		return c, fmt.Errorf("encrypt customer email: %w", err)
	}
	if c.DisplayName, err = s.enc.Encrypt(c.DisplayName); err != nil {
		return c, fmt.Errorf("encrypt customer display_name: %w", err)
	}
	return c, nil
}

// decryptCustomer decrypts PII fields on a Customer after reading from the DB.
func (s *PostgresStore) decryptCustomer(c domain.Customer) (domain.Customer, error) {
	if s.enc == nil {
		return c, nil
	}
	var err error
	if c.Email, err = s.enc.Decrypt(c.Email); err != nil {
		return c, fmt.Errorf("decrypt customer email: %w", err)
	}
	if c.DisplayName, err = s.enc.Decrypt(c.DisplayName); err != nil {
		return c, fmt.Errorf("decrypt customer display_name: %w", err)
	}
	return c, nil
}

// additionalEmailsStored serializes the CC list for the
// additional_emails column: JSON array, encrypted under the same
// encryptor as the sibling email column (plaintext JSON when no
// encryptor is configured), ” for an empty list. A DB-level shape
// check is impossible on ciphertext — the service layer owns
// validation and the cap.
func (s *PostgresStore) additionalEmailsStored(list []string) (string, error) {
	if len(list) == 0 {
		return "", nil
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return "", fmt.Errorf("marshal additional_emails: %w", err)
	}
	if s.enc == nil {
		return string(raw), nil
	}
	ct, err := s.enc.Encrypt(string(raw))
	if err != nil {
		return "", fmt.Errorf("encrypt additional_emails: %w", err)
	}
	return ct, nil
}

// additionalEmailsLoaded is the read-side inverse (Decrypt passes
// plaintext values through, same backward-compat as email).
func (s *PostgresStore) additionalEmailsLoaded(stored string) ([]string, error) {
	if stored == "" {
		return nil, nil
	}
	if s.enc != nil {
		pt, err := s.enc.Decrypt(stored)
		if err != nil {
			return nil, fmt.Errorf("decrypt additional_emails: %w", err)
		}
		stored = pt
	}
	var out []string
	if err := json.Unmarshal([]byte(stored), &out); err != nil {
		return nil, fmt.Errorf("decode additional_emails: %w", err)
	}
	return out, nil
}

// encryptBillingProfile encrypts PII fields on a BillingProfile before writing.
func (s *PostgresStore) encryptBillingProfile(bp domain.CustomerBillingProfile) (domain.CustomerBillingProfile, error) {
	if s.enc == nil {
		return bp, nil
	}
	var err error
	if bp.LegalName, err = s.enc.Encrypt(bp.LegalName); err != nil {
		return bp, fmt.Errorf("encrypt billing legal_name: %w", err)
	}
	if bp.Phone, err = s.enc.Encrypt(bp.Phone); err != nil {
		return bp, fmt.Errorf("encrypt billing phone: %w", err)
	}
	if bp.TaxID, err = s.enc.Encrypt(bp.TaxID); err != nil {
		return bp, fmt.Errorf("encrypt billing tax_id: %w", err)
	}
	return bp, nil
}

// decryptBillingProfile decrypts PII fields on a BillingProfile after reading.
func (s *PostgresStore) decryptBillingProfile(bp domain.CustomerBillingProfile) (domain.CustomerBillingProfile, error) {
	if s.enc == nil {
		return bp, nil
	}
	var err error
	if bp.LegalName, err = s.enc.Decrypt(bp.LegalName); err != nil {
		return bp, fmt.Errorf("decrypt billing legal_name: %w", err)
	}
	if bp.Phone, err = s.enc.Decrypt(bp.Phone); err != nil {
		return bp, fmt.Errorf("decrypt billing phone: %w", err)
	}
	if bp.TaxID, err = s.enc.Decrypt(bp.TaxID); err != nil {
		return bp, fmt.Errorf("decrypt billing tax_id: %w", err)
	}
	return bp, nil
}

func (s *PostgresStore) Create(ctx context.Context, tenantID string, c domain.Customer) (domain.Customer, error) {
	return s.CreateAudited(ctx, tenantID, c, nil)
}

// CreateAudited inserts the customer and runs the caller-supplied audit
// emission on the SAME transaction (ADR-090 shared fate): a committed
// customer with no audit row — and an audit row for a customer whose INSERT
// lost a unique-violation race — are both unrepresentable. The emission runs
// only after the INSERT's RETURNING has yielded a row, so it can never
// fabricate evidence of a create that didn't happen; an emit error aborts the
// whole tx (the customer is NOT created).
func (s *PostgresStore) CreateAudited(ctx context.Context, tenantID string, c domain.Customer, emit func(tx *sql.Tx, out domain.Customer) error) (domain.Customer, error) {
	enc, err := s.encryptCustomer(c)
	if err != nil {
		return domain.Customer{}, err
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Customer{}, err
	}
	defer postgres.Rollback(tx)

	id := postgres.NewID("vlx_cus")
	now := clock.Now(ctx)

	storedCC, err := s.additionalEmailsStored(c.AdditionalEmails)
	if err != nil {
		return domain.Customer{}, err
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO customers (id, tenant_id, external_id, display_name, email, email_bidx, additional_emails, status, test_clock_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6,''), $7, $8, NULLIF($9,''), $10, $10)
		RETURNING id, tenant_id, external_id, display_name, email, status, COALESCE(test_clock_id,''), created_at, updated_at
	`, id, tenantID, c.ExternalID, enc.DisplayName, enc.Email,
		s.emailBlindIndex(c.Email), storedCC, domain.CustomerStatusActive, c.TestClockID, now,
	).Scan(&c.ID, &c.TenantID, &c.ExternalID, &c.DisplayName, &c.Email, &c.Status, &c.TestClockID, &c.CreatedAt, &c.UpdatedAt)

	if err != nil {
		if postgres.IsUniqueViolation(err) {
			return domain.Customer{}, errs.AlreadyExists("external_id", fmt.Sprintf("customer with external_id %q already exists", c.ExternalID))
		}
		return domain.Customer{}, err
	}

	c, err = s.decryptCustomer(c)
	if err != nil {
		return domain.Customer{}, err
	}

	// Emit on the INSERT's own tx, with the RETURNING-scanned (and
	// decrypted) row — the audit label/flags describe what actually landed,
	// never the pre-write input.
	if emit != nil {
		if err := emit(tx, c); err != nil {
			return domain.Customer{}, fmt.Errorf("audit emission: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return domain.Customer{}, err
	}
	return c, nil
}

func (s *PostgresStore) Get(ctx context.Context, tenantID, id string) (domain.Customer, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Customer{}, err
	}
	defer postgres.Rollback(tx)

	var c domain.Customer
	var storedCC string
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, external_id, display_name, COALESCE(email, ''), status,
			email_status, email_last_bounced_at, COALESCE(email_bounce_reason,''),
			COALESCE(test_clock_id, ''),
			COALESCE(dunning_policy_id, ''),
			COALESCE(stripe_customer_id, ''),
			additional_emails,
			created_at, updated_at
		FROM customers WHERE id = $1
	`, id).Scan(&c.ID, &c.TenantID, &c.ExternalID, &c.DisplayName, &c.Email, &c.Status,
		(*string)(&c.EmailStatus), &c.EmailLastBouncedAt, &c.EmailBounceReason,
		&c.TestClockID,
		&c.DunningPolicyID,
		&c.StripeCustomerID,
		&storedCC,
		&c.CreatedAt, &c.UpdatedAt)

	if err == sql.ErrNoRows {
		return domain.Customer{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Customer{}, err
	}
	if c.AdditionalEmails, err = s.additionalEmailsLoaded(storedCC); err != nil {
		return domain.Customer{}, err
	}
	return s.decryptCustomer(c)
}

// SetStripeCustomerID writes (or backfills) the Stripe Customer ID
// against this Velox customer. Idempotent via the partial unique
// index (migration 0096) — a re-write of the same value is a no-op
// and a write of a different value to a row that already has one
// returns a unique-violation error so callers can read the existing
// value rather than blow it away.
//
// Single writer for customers.stripe_customer_id since the
// customer_payment_setups summary table was retired (migration
// 0097). Called by paymentmethods.StripeAdapter.EnsureStripeCustomer
// on the lazy-create path and by the legacy /v1/checkout/setup
// operator flow on initial bootstrap.
func (s *PostgresStore) SetStripeCustomerID(ctx context.Context, tenantID, customerID, stripeCustomerID string) error {
	return s.SetStripeCustomerIDAudited(ctx, tenantID, customerID, stripeCustomerID, nil)
}

// SetStripeCustomerIDAudited runs the caller-supplied audit emission on the
// same tx as the mapping write (ADR-090). The checkout.session.completed
// payment-setup flip uses it — previously that background webhook mutation
// left no audit trail at all.
func (s *PostgresStore) SetStripeCustomerIDAudited(ctx context.Context, tenantID, customerID, stripeCustomerID string, emit func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE customers
		SET stripe_customer_id = $1, updated_at = $2
		WHERE id = $3 AND tenant_id = $4
	`, stripeCustomerID, clock.Now(ctx), customerID, tenantID)
	if err != nil {
		return fmt.Errorf("set stripe_customer_id: %w", err)
	}
	// A zero-row UPDATE means the customer isn't there to map (unknown id, the
	// other livemode plane under RLS, or torn down by a test-clock delete
	// before a late webhook). Report it as ErrNotFound and emit NOTHING: the
	// mutation did not happen, so evidence of it would be fabricated, and a
	// silent success would let the caller hand back a Stripe session for a
	// customer Velox does not have.
	//
	// The two callers want opposite handling and BOTH are correct, so the
	// store reports the fact and lets them decide: the operator checkout route
	// turns it into a 404, while the checkout.session.completed webhook
	// tolerates it (acking the event rather than looping Stripe's retries
	// forever against a customer that no longer exists).
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set stripe_customer_id: rows affected: %w", err)
	}
	if n == 0 {
		return errs.ErrNotFound
	}
	if emit != nil {
		if err := emit(tx); err != nil {
			return fmt.Errorf("audit emission: %w", err)
		}
	}
	return tx.Commit()
}

// SetCostDashboardToken writes (or rotates) the cost-dashboard public
// token on a customer row. The caller is responsible for minting a
// fresh token via NewCostDashboardToken before invoking this. Old
// tokens are discarded immediately — there's no grace window, since
// the public dashboard is read-only and rotating because of a leak
// must take effect now. Per ADR-031 / cost-dashboard rollout: this
// is the only writer for customers.cost_dashboard_token_hash.
//
// Only the SHA-256 is persisted (migration 0172). The raw token leaves
// this process exactly once, in the rotate response — so this method
// takes the raw token and drops it, rather than accepting a pre-hashed
// value: leaving the hashing to callers is how one caller eventually
// forgets and writes plaintext.
func (s *PostgresStore) SetCostDashboardToken(ctx context.Context, tenantID, customerID, token string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	result, err := tx.ExecContext(ctx, `
		UPDATE customers SET cost_dashboard_token_hash = $1, updated_at = now()
		WHERE id = $2
	`, HashCostDashboardToken(token), customerID)
	if err != nil {
		return fmt.Errorf("set cost_dashboard_token_hash: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return errs.ErrNotFound
	}
	return tx.Commit()
}

// GetByCostDashboardToken resolves a customer from the public cost-
// dashboard token. Uses TxBypass because the public route has no
// tenant context yet — the token IS the credential, and lookup
// must succeed before any tenant scoping. The returned customer's
// tenant_id is what subsequent calls scope to. Tokens are 64 random
// hex chars (32 bytes entropy via NewCostDashboardToken) so guessing
// is computationally infeasible; the partial UNIQUE index on
// cost_dashboard_token_hash IS NOT NULL prevents collisions at write.
//
// The presented token is hashed before the comparison — the column holds
// only the blind index (migration 0172), so an equality match on the raw
// token would silently never resolve.
func (s *PostgresStore) GetByCostDashboardToken(ctx context.Context, token string) (domain.Customer, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return domain.Customer{}, err
	}
	defer postgres.Rollback(tx)

	var c domain.Customer
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, external_id, display_name, COALESCE(email, ''), status,
			email_status, email_last_bounced_at, COALESCE(email_bounce_reason,''),
			COALESCE(test_clock_id, ''),
			COALESCE(dunning_policy_id, ''),
			livemode,
			created_at, updated_at
		FROM customers WHERE cost_dashboard_token_hash = $1
	`, HashCostDashboardToken(token)).Scan(&c.ID, &c.TenantID, &c.ExternalID, &c.DisplayName, &c.Email, &c.Status,
		(*string)(&c.EmailStatus), &c.EmailLastBouncedAt, &c.EmailBounceReason,
		&c.TestClockID,
		&c.DunningPolicyID,
		&c.Livemode,
		&c.CreatedAt, &c.UpdatedAt)

	if err == sql.ErrNoRows {
		return domain.Customer{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Customer{}, err
	}
	return s.decryptCustomer(c)
}

func (s *PostgresStore) GetByExternalID(ctx context.Context, tenantID, externalID string) (domain.Customer, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Customer{}, err
	}
	defer postgres.Rollback(tx)

	var c domain.Customer
	var storedCC string
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, external_id, display_name, COALESCE(email, ''), status,
			email_status, email_last_bounced_at, COALESCE(email_bounce_reason,''),
			COALESCE(test_clock_id, ''),
			COALESCE(dunning_policy_id, ''),
			additional_emails,
			created_at, updated_at
		FROM customers WHERE external_id = $1
	`, externalID).Scan(&c.ID, &c.TenantID, &c.ExternalID, &c.DisplayName, &c.Email, &c.Status,
		(*string)(&c.EmailStatus), &c.EmailLastBouncedAt, &c.EmailBounceReason,
		&c.TestClockID,
		&c.DunningPolicyID,
		&storedCC,
		&c.CreatedAt, &c.UpdatedAt)

	if err == sql.ErrNoRows {
		return domain.Customer{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Customer{}, err
	}
	if c.AdditionalEmails, err = s.additionalEmailsLoaded(storedCC); err != nil {
		return domain.Customer{}, err
	}
	return s.decryptCustomer(c)
}

// GetByStripeCustomerID resolves a customer from its Stripe Customer ID.
// The (tenant_id, livemode, stripe_customer_id) index is unique, so the
// TxTenant RLS scope (tenant + livemode) makes this a 1:1 lookup. Used by
// the setup_intent.succeeded webhook to map the SetupIntent's authoritative
// `customer` field back to the Velox customer when the SetupIntent carries
// no velox_customer_id metadata (Stripe Checkout doesn't copy session
// metadata onto the SetupIntent). An empty id never matches — the partial
// unique index excludes blank ids — so we fail closed rather than scan.
func (s *PostgresStore) GetByStripeCustomerID(ctx context.Context, tenantID, stripeCustomerID string) (domain.Customer, error) {
	if stripeCustomerID == "" {
		return domain.Customer{}, errs.ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Customer{}, err
	}
	defer postgres.Rollback(tx)

	var c domain.Customer
	err = tx.QueryRowContext(ctx, `
		SELECT id, tenant_id, external_id, display_name, COALESCE(email, ''), status,
			email_status, email_last_bounced_at, COALESCE(email_bounce_reason,''),
			COALESCE(test_clock_id, ''),
			COALESCE(dunning_policy_id, ''),
			created_at, updated_at
		FROM customers WHERE stripe_customer_id = $1
	`, stripeCustomerID).Scan(&c.ID, &c.TenantID, &c.ExternalID, &c.DisplayName, &c.Email, &c.Status,
		(*string)(&c.EmailStatus), &c.EmailLastBouncedAt, &c.EmailBounceReason,
		&c.TestClockID,
		&c.DunningPolicyID,
		&c.CreatedAt, &c.UpdatedAt)

	if err == sql.ErrNoRows {
		return domain.Customer{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Customer{}, err
	}
	return s.decryptCustomer(c)
}

// EmailMatch is the narrow result row from FindByEmailBlindIndex — enough
// identity to mint a magic link but no PII. Returned cross-tenant, so the
// caller (the public magic-link handler) iterates one match per (tenant,
// customer) and never leaks details across boundaries.
type EmailMatch struct {
	TenantID   string
	CustomerID string
	Livemode   bool
	Status     string
}

// FindByEmailBlindIndex resolves the email → customer(s) lookup the public
// magic-link endpoint needs. Runs under TxBypass because the caller is
// unauthenticated until this returns — the blind index itself is the only
// handle we have, and an attacker who can't compute HMAC(key, email)
// can't enumerate. Callers must pre-compute the blind index via their own
// crypto.Blinder instance so this method stays a pure DB read.
//
// Returns at most `limit` matches (sane ceiling so a colliding index can
// never fan out into thousands of emails). Empty blind → no matches,
// regardless of DB state, so misconfigured blinders silently fail closed.
func (s *PostgresStore) FindByEmailBlindIndex(ctx context.Context, blind string, limit int) ([]EmailMatch, error) {
	if blind == "" {
		return nil, nil
	}
	// Default 10 for unset/invalid; clamp to 50 when over-cap. Was
	// silently truncating to 10 on >50 asks — see invoice/postgres.go
	// for the rationale (no-silent-fallbacks principle, 2026-05-28).
	if limit <= 0 {
		limit = 10
	} else if limit > 50 {
		limit = 50
	}
	tx, err := s.db.BeginTx(ctx, postgres.TxBypass, "")
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT tenant_id, id, livemode, status
		FROM customers
		WHERE email_bidx = $1 AND status = 'active'
		ORDER BY tenant_id, id
		LIMIT $2
	`, blind, limit)
	if err != nil {
		return nil, fmt.Errorf("select: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []EmailMatch
	for rows.Next() {
		var m EmailMatch
		if err := rows.Scan(&m.TenantID, &m.CustomerID, &m.Livemode, &m.Status); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func (s *PostgresStore) List(ctx context.Context, filter ListFilter) ([]domain.Customer, int, error) {
	if strings.TrimSpace(filter.Search) != "" {
		return s.listSearch(ctx, filter)
	}
	// Encrypted-column sorts can't be done in SQL (ORDER BY would order
	// ciphertext). With encryption active, take the decrypt-then-sort
	// path; without it (self-host with no encryption key) the columns
	// are plaintext and the SQL sort below is correct and cheaper.
	if k := filter.Sort; (k == "display_name" || k == "name" || k == "email") && s.enc != nil && s.enc.IsEnabled() {
		return s.listSortedByEncryptedColumn(ctx, filter)
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	// Default 50 for unset/invalid; clamp to 100 when over-cap. See
	// invoice/postgres.go for the no-silent-fallbacks rationale.
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}

	where, args := buildCustomerWhere(filter)

	var total int
	countQuery := `SELECT COUNT(*) FROM customers` + where
	if err := tx.QueryRowContext(ctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	query := `SELECT id, tenant_id, external_id, display_name, COALESCE(email, ''), status, COALESCE(test_clock_id,''), created_at, updated_at
		FROM customers` + where +
		` ORDER BY ` + customerOrderBy(filter.Sort, filter.SortDir) +
		` LIMIT $` + fmt.Sprintf("%d", len(args)+1) + ` OFFSET $` + fmt.Sprintf("%d", len(args)+2)
	args = append(args, limit, filter.Offset)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var customers []domain.Customer
	for rows.Next() {
		var c domain.Customer
		if err := rows.Scan(&c.ID, &c.TenantID, &c.ExternalID, &c.DisplayName, &c.Email, &c.Status, &c.TestClockID, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, 0, err
		}
		c, err = s.decryptCustomer(c)
		if err != nil {
			return nil, 0, err
		}
		customers = append(customers, c)
	}
	return customers, total, rows.Err()
}

// searchScanCap bounds the decrypt-and-match scan backing
// ListFilter.Search. display_name and email are encrypted at rest
// (SetEncryptor), so SQL ILIKE can't see them — the only correct
// substring match is post-decrypt in Go. The email blind index
// (email_bidx) is equality-only and doesn't help a search box.
// At pre-launch tenant sizes (hundreds of customers) the scan is
// trivially cheap; if a tenant ever approaches this cap, the upgrade
// path is a trigram-indexed search-token column, not a bigger cap.
const searchScanCap = 5000

// listSearch is the Search != "" branch of List. It applies the
// SQL-expressible filters (status, external_id, ids) and ORDER BY in
// the database, streams up to searchScanCap rows, decrypts, and
// substring-matches display_name / email / external_id / id in Go.
// Pagination (total + offset/limit window) runs over the matched set
// so the dashboard's "Showing X–Y of N" stays truthful.
func (s *PostgresStore) listSearch(ctx context.Context, filter ListFilter) ([]domain.Customer, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}

	where, args := buildCustomerWhere(filter)
	query := `SELECT id, tenant_id, external_id, display_name, COALESCE(email, ''), status, COALESCE(test_clock_id,''), created_at, updated_at
		FROM customers` + where +
		` ORDER BY ` + customerOrderBy(filter.Sort, filter.SortDir) +
		` LIMIT $` + fmt.Sprintf("%d", len(args)+1)
	args = append(args, searchScanCap)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	q := strings.ToLower(strings.TrimSpace(filter.Search))
	var matched []domain.Customer
	for rows.Next() {
		var c domain.Customer
		if err := rows.Scan(&c.ID, &c.TenantID, &c.ExternalID, &c.DisplayName, &c.Email, &c.Status, &c.TestClockID, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, 0, err
		}
		c, err = s.decryptCustomer(c)
		if err != nil {
			return nil, 0, err
		}
		if strings.Contains(strings.ToLower(c.DisplayName), q) ||
			strings.Contains(strings.ToLower(c.Email), q) ||
			strings.Contains(strings.ToLower(c.ExternalID), q) ||
			strings.Contains(strings.ToLower(c.ID), q) {
			matched = append(matched, c)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// display_name / email sort keys order by CIPHERTEXT in SQL (the
	// columns are encrypted), which is meaningless to an operator. On
	// this path the rows are already decrypted, so re-sort in Go with
	// the same dir + id tie-break contract as customerOrderBy.
	sortCustomersByPlaintext(matched, filter.Sort, filter.SortDir)

	total := len(matched)
	start := filter.Offset
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	return matched[start:end], total, nil
}

// sortCustomersByPlaintext re-sorts already-decrypted customers in Go for
// the sort keys whose DB columns are encrypted (display_name / email) —
// SQL ORDER BY on those columns orders ciphertext, which reads as random
// to an operator. No-op for other keys. Same dir + id tie-break contract
// as customerOrderBy.
func sortCustomersByPlaintext(customers []domain.Customer, key, dir string) {
	if key != "display_name" && key != "name" && key != "email" {
		return
	}
	desc := dir != "asc"
	val := func(c domain.Customer) string {
		if key == "email" {
			return strings.ToLower(c.Email)
		}
		return strings.ToLower(c.DisplayName)
	}
	sort.SliceStable(customers, func(i, j int) bool {
		a, b := val(customers[i]), val(customers[j])
		if a != b {
			if desc {
				return a > b
			}
			return a < b
		}
		if desc {
			return customers[i].ID > customers[j].ID
		}
		return customers[i].ID < customers[j].ID
	})
}

// listSortedByEncryptedColumn serves the plain (no-search) list when the
// requested sort key lives in an encrypted column: a bounded scan
// (searchScanCap most-recent rows), decrypt, sort by plaintext in Go,
// paginate in memory. Mirrors listSearch's trade — exact under the cap,
// window-truncated above it — and shares its upgrade path (a derived
// sort-token column) when a tenant outgrows the cap. Without this path
// the list page's name/email sort ordered rows by ciphertext.
func (s *PostgresStore) listSortedByEncryptedColumn(ctx context.Context, filter ListFilter) ([]domain.Customer, int, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, filter.TenantID)
	if err != nil {
		return nil, 0, err
	}
	defer postgres.Rollback(tx)

	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	} else if limit > 100 {
		limit = 100
	}

	where, args := buildCustomerWhere(filter)
	// Deterministic scan window: the searchScanCap most recent rows.
	query := `SELECT id, tenant_id, external_id, display_name, COALESCE(email, ''), status, COALESCE(test_clock_id,''), created_at, updated_at
		FROM customers` + where +
		` ORDER BY created_at DESC, id DESC LIMIT $` + fmt.Sprintf("%d", len(args)+1)
	args = append(args, searchScanCap)

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()

	var customers []domain.Customer
	for rows.Next() {
		var c domain.Customer
		if err := rows.Scan(&c.ID, &c.TenantID, &c.ExternalID, &c.DisplayName, &c.Email, &c.Status, &c.TestClockID, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, 0, err
		}
		c, err = s.decryptCustomer(c)
		if err != nil {
			return nil, 0, err
		}
		customers = append(customers, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	sortCustomersByPlaintext(customers, filter.Sort, filter.SortDir)

	total := len(customers)
	start := filter.Offset
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}
	return customers[start:end], total, nil
}

// ListByTestClockID returns customers pinned to the given test clock,
// fully decrypted. The testclock domain calls this through the
// CustomerReader interface so it never reaches into the customers
// table directly — keeping decryption centralised on this read path
// (encrypt-at-rest is otherwise transparently bypassed when another
// package SELECTs display_name / email straight from the table).
func (s *PostgresStore) ListByTestClockID(ctx context.Context, tenantID, clockID string) ([]domain.Customer, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return nil, err
	}
	defer postgres.Rollback(tx)

	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, external_id, display_name, COALESCE(email, ''), status,
			COALESCE(test_clock_id, ''),
			COALESCE(dunning_policy_id, ''),
			created_at, updated_at
		FROM customers
		WHERE test_clock_id = $1
		ORDER BY created_at ASC
		LIMIT 1000
	`, clockID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var customers []domain.Customer
	for rows.Next() {
		var c domain.Customer
		if err := rows.Scan(&c.ID, &c.TenantID, &c.ExternalID, &c.DisplayName, &c.Email, &c.Status,
			&c.TestClockID, &c.DunningPolicyID, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c, err = s.decryptCustomer(c)
		if err != nil {
			return nil, err
		}
		customers = append(customers, c)
	}
	return customers, rows.Err()
}

func (s *PostgresStore) Update(ctx context.Context, tenantID string, c domain.Customer) (domain.Customer, error) {
	enc, err := s.encryptCustomer(c)
	if err != nil {
		return domain.Customer{}, err
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.Customer{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	storedCC, err := s.additionalEmailsStored(c.AdditionalEmails)
	if err != nil {
		return domain.Customer{}, err
	}
	// Update intentionally does NOT touch test_clock_id — Stripe parity:
	// once a customer is attached to a clock at create time, they're
	// pinned for life. To switch clocks, delete + recreate. The column
	// is read back into the result so callers see the unchanged value.
	err = tx.QueryRowContext(ctx, `
		UPDATE customers SET display_name = $1, email = $2, email_bidx = NULLIF($3,''),
			additional_emails = $4, status = $5, dunning_policy_id = NULLIF($6,''), updated_at = $7
		WHERE id = $8
		RETURNING id, tenant_id, external_id, display_name, email, status, COALESCE(test_clock_id,''), COALESCE(dunning_policy_id,''), created_at, updated_at
	`, enc.DisplayName, enc.Email, s.emailBlindIndex(c.Email), storedCC, c.Status, c.DunningPolicyID, now, c.ID,
	).Scan(&c.ID, &c.TenantID, &c.ExternalID, &c.DisplayName, &c.Email, &c.Status, &c.TestClockID, &c.DunningPolicyID, &c.CreatedAt, &c.UpdatedAt)

	if err == sql.ErrNoRows {
		return domain.Customer{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.Customer{}, err
	}

	c, err = s.decryptCustomer(c)
	if err != nil {
		return domain.Customer{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.Customer{}, err
	}
	return c, nil
}

// ResetEmailStatus clears any prior bounce/complain state on the
// customer. Called by the service layer when the email value changes
// (operator edit, portal self-edit, billing-profile email change that
// syncs down) — without this reset, a bounced flag on the OLD address
// would silently suppress sends to a brand-new untested address.
//
// Sets email_status to 'unknown' (not 'ok') because we don't actually
// know the new address works yet; the next successful dispatch is
// what flips it to 'ok' (future change). Idempotent: re-resetting an
// already-unknown row is a no-op write.
func (s *PostgresStore) ResetEmailStatus(ctx context.Context, tenantID, customerID string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)
	if _, err := tx.ExecContext(ctx, `
		UPDATE customers
		SET email_status = 'unknown',
		    email_bounce_reason = NULL,
		    email_last_bounced_at = NULL,
		    updated_at = NOW()
		WHERE id = $1
	`, customerID); err != nil {
		return fmt.Errorf("reset email status: %w", err)
	}
	return tx.Commit()
}

// MarkEmailBounced flips email_status to 'bounced' and records the
// timestamp + free-text reason. Accepts customerID (not email) to avoid
// routing a raw email string through the store — the caller holds the
// blind-index lookup, which keeps encrypted email handling in one place.
// Idempotent: repeated calls refresh the timestamp and reason but are
// otherwise no-ops. NEVER downgrades 'complained' (ADR-098): a spam
// complaint outranks a bounce, and provider webhooks arrive in any order
// — a Bounce landing after a SpamComplaint is a benign no-op, not an
// ErrNotFound (the SMTP-path caller logs ErrNotFound as a real miss).
func (s *PostgresStore) MarkEmailBounced(ctx context.Context, tenantID, customerID, reason string) error {
	return s.markEmailSuppressed(ctx, tenantID, customerID, reason, string(domain.EmailStatusBounced))
}

// MarkEmailComplained flips email_status to 'complained' — the top of the
// suppression lattice (unknown < ok < bounced < complained). Sole caller
// is the provider's SpamComplaint webhook (ADR-098; the state 0050
// anticipated but nothing wrote until now): a complaint is at least as
// severe as a hard bounce, and provider-side it is effectively
// irreversible. Idempotent; never downgraded by later bounces.
func (s *PostgresStore) MarkEmailComplained(ctx context.Context, tenantID, customerID, reason string) error {
	return s.markEmailSuppressed(ctx, tenantID, customerID, reason, string(domain.EmailStatusComplained))
}

func (s *PostgresStore) markEmailSuppressed(ctx context.Context, tenantID, customerID, reason, status string) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	res, err := tx.ExecContext(ctx, `
		UPDATE customers SET email_status = $3,
			email_last_bounced_at = NOW(),
			email_bounce_reason = NULLIF($1, ''),
			updated_at = NOW()
		WHERE id = $2 AND email_status <> 'complained'
	`, reason, customerID, status)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Distinguish "row absent" (a real miss the caller should hear
		// about) from "row present but already 'complained'" (the
		// monotonic guard blocked a downgrade — benign no-op).
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM customers WHERE id = $1)`, customerID,
		).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errs.ErrNotFound
		}
		return nil
	}
	return tx.Commit()
}

func (s *PostgresStore) UpsertBillingProfile(ctx context.Context, tenantID string, bp domain.CustomerBillingProfile) (domain.CustomerBillingProfile, error) {
	enc, err := s.encryptBillingProfile(bp)
	if err != nil {
		return domain.CustomerBillingProfile{}, err
	}

	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CustomerBillingProfile{}, err
	}
	defer postgres.Rollback(tx)

	now := clock.Now(ctx)
	status := string(bp.TaxStatus)
	if status == "" {
		status = "standard"
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO customer_billing_profiles (customer_id, tenant_id, legal_name, phone,
			address_line1, address_line2, city, state, postal_code, country, currency,
			tax_status, tax_exempt_reason, tax_id, tax_id_type,
			profile_status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $17)
		ON CONFLICT (tenant_id, customer_id) DO UPDATE SET
			legal_name = EXCLUDED.legal_name, phone = EXCLUDED.phone,
			address_line1 = EXCLUDED.address_line1, address_line2 = EXCLUDED.address_line2,
			city = EXCLUDED.city, state = EXCLUDED.state, postal_code = EXCLUDED.postal_code,
			country = EXCLUDED.country, currency = EXCLUDED.currency,
			tax_status = EXCLUDED.tax_status,
			tax_exempt_reason = EXCLUDED.tax_exempt_reason,
			tax_id = EXCLUDED.tax_id, tax_id_type = EXCLUDED.tax_id_type,
			profile_status = EXCLUDED.profile_status, updated_at = EXCLUDED.updated_at
		RETURNING customer_id, tenant_id, COALESCE(legal_name,''), COALESCE(phone,''),
			COALESCE(address_line1,''), COALESCE(address_line2,''), COALESCE(city,''), COALESCE(state,''),
			COALESCE(postal_code,''), COALESCE(country,''), COALESCE(currency,''),
			tax_status, COALESCE(tax_exempt_reason,''),
			COALESCE(tax_id,''), COALESCE(tax_id_type,''),
			profile_status, created_at, updated_at
	`, bp.CustomerID, tenantID, postgres.NullableString(enc.LegalName),
		postgres.NullableString(enc.Phone), postgres.NullableString(bp.AddressLine1),
		postgres.NullableString(bp.AddressLine2), postgres.NullableString(bp.City),
		postgres.NullableString(bp.State), postgres.NullableString(bp.PostalCode),
		postgres.NullableString(bp.Country), postgres.NullableString(bp.Currency),
		status, bp.TaxExemptReason, enc.TaxID, bp.TaxIDType,
		bp.ProfileStatus, now,
	).Scan(
		&bp.CustomerID, &bp.TenantID, &bp.LegalName, &bp.Phone,
		&bp.AddressLine1, &bp.AddressLine2, &bp.City, &bp.State, &bp.PostalCode,
		&bp.Country, &bp.Currency, &bp.TaxStatus, &bp.TaxExemptReason,
		&bp.TaxID, &bp.TaxIDType,
		&bp.ProfileStatus, &bp.CreatedAt, &bp.UpdatedAt,
	)
	if err != nil {
		return domain.CustomerBillingProfile{}, err
	}

	bp, err = s.decryptBillingProfile(bp)
	if err != nil {
		return domain.CustomerBillingProfile{}, err
	}

	if err := tx.Commit(); err != nil {
		return domain.CustomerBillingProfile{}, err
	}
	return bp, nil
}

func (s *PostgresStore) GetBillingProfile(ctx context.Context, tenantID, customerID string) (domain.CustomerBillingProfile, error) {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return domain.CustomerBillingProfile{}, err
	}
	defer postgres.Rollback(tx)

	var bp domain.CustomerBillingProfile
	err = tx.QueryRowContext(ctx, `
		SELECT customer_id, tenant_id, COALESCE(legal_name,''), COALESCE(phone,''),
			COALESCE(address_line1,''), COALESCE(address_line2,''), COALESCE(city,''), COALESCE(state,''),
			COALESCE(postal_code,''), COALESCE(country,''), COALESCE(currency,''),
			tax_status, COALESCE(tax_exempt_reason,''),
			COALESCE(tax_id,''), COALESCE(tax_id_type,''),
			profile_status, created_at, updated_at
		FROM customer_billing_profiles WHERE customer_id = $1
	`, customerID).Scan(
		&bp.CustomerID, &bp.TenantID, &bp.LegalName, &bp.Phone,
		&bp.AddressLine1, &bp.AddressLine2, &bp.City, &bp.State, &bp.PostalCode,
		&bp.Country, &bp.Currency, &bp.TaxStatus, &bp.TaxExemptReason,
		&bp.TaxID, &bp.TaxIDType,
		&bp.ProfileStatus, &bp.CreatedAt, &bp.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return domain.CustomerBillingProfile{}, errs.ErrNotFound
	}
	if err != nil {
		return domain.CustomerBillingProfile{}, err
	}
	return s.decryptBillingProfile(bp)
}

// UpsertPaymentSetup / GetPaymentSetup REMOVED. The customer_payment_setups
// table was dropped in migration 0097; callers that need the wire shape
// use compositePaymentSetupStore (internal/api/adapters.go) which composes
// the response from canonical sources (customers + payment_methods).

// customerOrderBy validates sort + dir against a closed allow-list
// and adds a deterministic id tie-break matching the primary
// direction. See invoiceOrderBy for the design rationale.
func customerOrderBy(sort, dir string) string {
	col := customerSortColumn(sort)
	d := "DESC"
	if dir == "asc" {
		d = "ASC"
	}
	return col + " " + d + ", id " + d
}

func customerSortColumn(key string) string {
	switch key {
	case "display_name", "name":
		return "display_name"
	case "email":
		return "email"
	case "external_id":
		return "external_id"
	case "status":
		return "status"
	default:
		return "created_at"
	}
}

func buildCustomerWhere(f ListFilter) (string, []any) {
	var clauses []string
	var args []any
	idx := 1

	if f.Status != "" {
		clauses = append(clauses, fmt.Sprintf("status = $%d", idx))
		args = append(args, f.Status)
		idx++
	}
	if f.ExternalID != "" {
		clauses = append(clauses, fmt.Sprintf("external_id = $%d", idx))
		args = append(args, f.ExternalID)
		idx++
	}
	if len(f.IDs) > 0 {
		// ids=... filter used by other list pages to fetch exactly the
		// customers their primary rows reference (avoids the
		// "list-then-client-side-join" pagination bug). Bounded by
		// the upstream limit cap — 100 rows max keeps the IN clause
		// short and the query plan predictable.
		placeholders := make([]string, len(f.IDs))
		for i, id := range f.IDs {
			placeholders[i] = fmt.Sprintf("$%d", idx)
			args = append(args, id)
			idx++
		}
		clauses = append(clauses, "id IN ("+join(placeholders, ",")+")")
	}

	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + join(clauses, " AND "), args
}

func join(parts []string, sep string) string {
	result := ""
	for i, p := range parts {
		if i > 0 {
			result += sep
		}
		result += p
	}
	return result
}

// StreamForExport walks EVERY customer in the date range in ONE snapshot
// transaction, handing each row to fn as it is scanned.
//
// One transaction, one query, no pagination — because an export is an ARTIFACT,
// and an artifact assembled from N independent transactions is not a picture of
// the data at any single moment. The export used to page with LIMIT/OFFSET,
// each page in its own tx, over a newest-first ordering: a customer created
// while the export ran entered at the HEAD and shifted every later row down by
// one, so the row on each page boundary was written to the CSV TWICE (and a
// delete symmetrically SKIPPED one). That file is what an operator hands to an
// auditor. Under a single MVCC snapshot the question cannot arise (issue #475).
//
// The SELECT is the EXPORT's, not List's. The export used to borrow List, whose
// SELECT omits email_status — so the `email_status` column in customers.csv was
// always empty: a field the artifact promised and never filled. An export's
// columns are its own contract; borrowing a list query is how they go blank.
//
// The date range is applied in SQL. It used to be applied in Go, AFTER the page
// was fetched, so a filtered export still walked every customer in the tenant.
func (s *PostgresStore) StreamForExport(ctx context.Context, tenantID string, from, to time.Time, fn func(domain.Customer) error) error {
	tx, err := s.db.BeginTx(ctx, postgres.TxTenant, tenantID)
	if err != nil {
		return err
	}
	defer postgres.Rollback(tx)

	// Tenant + livemode isolation is RLS's (TxTenant), exactly as in List — this
	// query adds no scoping of its own so the two cannot diverge on WHO can see
	// WHAT.
	where := ""
	var args []any
	if !from.IsZero() {
		args = append(args, from)
		where += fmt.Sprintf(" AND created_at >= $%d", len(args))
	}
	if !to.IsZero() {
		args = append(args, to)
		where += fmt.Sprintf(" AND created_at <= $%d", len(args))
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, tenant_id, external_id, display_name, COALESCE(email, ''),
			status, email_status, COALESCE(test_clock_id,''), created_at, updated_at
		FROM customers
		WHERE TRUE`+where+`
		ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var c domain.Customer
		if err := rows.Scan(&c.ID, &c.TenantID, &c.ExternalID, &c.DisplayName, &c.Email,
			&c.Status, &c.EmailStatus, &c.TestClockID, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return err
		}
		c, err = s.decryptCustomer(c)
		if err != nil {
			return err
		}
		if err := fn(c); err != nil {
			return err
		}
	}
	return rows.Err()
}
