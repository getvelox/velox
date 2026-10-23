# Glossary

Words used in the Velox docs and dashboard. For the big picture, read
[How Velox works](how-velox-works.md). Words from the code and ops docs:
[contributor glossary](dev/glossary.md).

## Accounts and access

- **tenant**: One business account in Velox, with its own customers, data,
  dashboard users and API keys. One Velox install can hold many tenants, and each sees only its own data.
  Dashboard users can belong to several tenants. [More](self-host.md)
- **operator**: Has two meanings. Usually the business that runs Velox to
  bill its customers, or its staff who sign in to the dashboard. In
  self-host and ops docs, the engineer who deploys and runs the install.
  [More](self-host.md)
- **customer**: A person or company that a tenant bills. Each customer
  belongs to one tenant and has an `external_id`: your own ID for it, such
  as the user ID in your database. Usage events use it to say whose usage it
  is. [More](api-surface.md)
- **test mode and live mode**: Two separate sets of data in one tenant, as
  in Stripe. Test mode is for trying things, and nothing in it charges real
  money. Live mode is real billing. An API key's mode (the `_test_` or
  `_live_` in its prefix) or the dashboard's mode switch picks which set you
  see. Tenant settings are shared, but each mode has its own Stripe
  connection. [More](api-keys.md)
- **API key types (secret, publishable, platform)**: The three kinds of
  key you send as `Authorization: Bearer <key>`. A secret key
  (`vlx_secret_`) has full access to one tenant and stays on your server. A
  publishable key (`vlx_pub_`) is safe in a browser, but reads no tenant
  data. A platform key (`vlx_platform_`) manages tenants and is not issued
  in the product. [More](api-keys.md)

## Pricing and usage

- **meter**: A counter for one kind of usage you charge for, such as tokens
  or API requests. Each meter has a unique `event_name`, a unit, one way to
  combine readings (its aggregation: `sum`, `count`, `max` or `last`), and
  optionally a default rating rule. Every price of the meter bills under its
  aggregation, which is fixed once the meter has a usage event (held usage
  does not count). Usage events name the meter by its `event_name`.
  [More](how-velox-works.md)
- **usage event**: One "this customer used this much" record that your app
  sends to `POST /v1/usage-events`. It names the customer and the meter and
  gives a decimal quantity. It carries a required idempotency key, so a
  resent event is not counted twice, and it can carry dimensions.
  [More](api-surface.md)
- **dimensions (on a usage event)**: Labels on a usage event, such as
  `model` or `token_type`, as key-value pairs. Pricing rules match on them,
  so one meter can be priced at many rates. Up to 16 keys, and each value is
  a single value (text, number, true/false or null), never a list or an
  object. [More](ai-pricing.md#how-velox-picks-the-price-for-an-event)
- **pricing rule**: A rule on a meter that picks the usage events to price.
  It takes the events whose dimensions contain every key-value pair in its
  `dimension_match`. It combines them into one quantity (sum, count, max or
  last value) and prices that quantity with a rating rule. If rules overlap,
  the highest-priority rule claims each event, so nothing counts twice.
  [More](ai-pricing.md#how-velox-picks-the-price-for-an-event)
- **rating rule**: A price formula that turns a usage quantity into money:
  a flat rate per unit, graduated tiers, or fixed-size packages. It is
  identified by its `rule_key` and has versions. Billing uses the version
  that was active when the period opened, so a new version takes effect
  next period. [More](adr/070-price-change-semantics.md)
- **plan**: A price package you sell: a currency, a billing interval
  (monthly or yearly), a fixed base fee, and the meters it charges usage
  for. A subscription has one or more items, each a plan with a quantity.
  A plan's status is active or archived. [More](features.md)
- **recipe**: A ready-made pricing setup that one API call applies. The
  built-in ones are `anthropic_style`, `openai_style` and
  `replicate_style`. A recipe creates meters, rating rules, pricing rules
  and plans, and sets the tenant's dunning policy. It also adds a webhook
  endpoint that stays inactive until you give it a real URL. Applying the
  same recipe again changes nothing. [More](quick-start.md#what-each-demo-step-does)
- **idempotency key**: A unique string that makes a retry safe: the same
  key means the same operation, so it takes effect once. The main kinds
  are the `Idempotency-Key` HTTP header, the key on each usage event, and
  the key Velox sends to Stripe on each charge. The header keeps one
  response for 24 hours; the usage routes ignore it. The usage-event key is
  required and kept forever, so a key used twice stores only the first
  event. [More](api-surface.md)

## Subscriptions and invoices

- **subscription**: A customer's agreement to pay for one or more plans,
  period after period. It holds items (each a plan and a quantity). Its
  status is draft, trialing, active or canceled.
  [More](features.md#subscriptions-and-invoices)
- **billing period**: The dates one subscription invoice covers. The start
  day is included and the end is not. Periods start and end at midnight in
  the tenant's billing timezone. A monthly period starts either on the 1st
  of the month (calendar billing, after a first partial period) or on the
  day the subscription started (anniversary billing). Yearly periods are
  always anniversary. [More](adr/012-day-grade-calendar-billing.md)
- **billing timezone**: The one timezone for your whole account (a tenant
  setting, UTC by default). Velox puts period starts and ends at midnight in
  it, and counts days for proration in it. Each invoice also keeps the zone
  it was issued under, so its dates never shift later.
  [More](adr/077-org-level-billing-timezone.md)
- **day-grade billing**: Billing counts whole days, not hours. Normal period
  starts and ends snap to midnight in the billing timezone, and the start
  day counts in full. Day counts are rounded, so daylight saving time never
  adds or loses a day. A spend threshold that restarts the cycle can start a
  period during the day. [More](adr/012-day-grade-calendar-billing.md)
- **bill timing (in advance / in arrears)**: A plan setting for when its
  fixed base fee is invoiced. In advance: at the start of the period it
  covers. In arrears: at the end. The default is in arrears. Usage is always
  billed in arrears. [More](adr/031-per-plan-base-bill-timing.md)
- **proration**: Charging only for the whole days a price was active in a
  billing period. For a plan billed in advance, a change in the middle of a
  period charges the difference now, or credits it on a downgrade, and a
  short first period is prorated when billed. A plan billed in arrears is
  prorated by days when the period closes.
  [More](adr/050-unpaid-source-proration-policy.md)
- **spend threshold**: A limit on one subscription that issues an invoice
  early. It is either a money amount (`amount_gte`) checked against the
  period's usage so far, or a usage count on one item (`usage_gte`).
  Crossing it issues an invoice with `billing_reason` "threshold" for that
  usage, and no base fee. The period keeps its dates, and the base fee is
  billed as it would be with no threshold.
  Usage is never blocked.
  [More](features.md#subscriptions-and-invoices)
- **invoice states (draft, finalized, paid, void, uncollectible)**: Where
  an invoice is in its life. Draft: you can still change it. Finalized:
  issued and owed. Paid. Void: canceled, as if the debt never existed (the
  API calls it `voided`). Uncollectible: written off as bad debt, but kept
  for the records. [More](adr/110-written-off-invoices-close-self-service-payment.md)
- **hosted invoice page**: A public web page for one issued invoice (not a
  draft). It opens from a secret link, so the customer needs no login. It
  shows the invoice and its PDF. While money is still owed, it has a Pay
  button that opens Stripe Checkout.
  [More](adr/021-hosted-invoice-pay-flow-saves-pm.md)

## Credits

- **credit grant / credit block**: Credit added to a customer's balance.
  Examples: a prepaid commit, free promotional credit, a goodwill credit,
  or credit from a proration or a credit note. The balance is a ledger of
  grant, usage, expiry and adjustment entries. Each grant is a block that
  invoices draw down when billed: promotional credit first, then the
  soonest to expire, then the oldest. A draw-down updates the grant entry's
  used amount in place. [More](adr/071-credit-expiry-retires-the-block.md)
- **prepaid commit**: Usage credit the customer pays for up front, sold as
  a line on a one-off invoice. When that invoice is finalized, Velox adds a
  credit block to the customer's balance. The credit can be more than the
  price (pay $9k, get $10k). The purchase is not taxed; the usage the
  credit pays for is. [More](ai-pricing.md#tax-on-prepaid-credit)
- **drawdown**: Using a customer's credit balance to pay part of an invoice
  when it is billed, so less is charged. Free promotional credit is used
  before paid credit. Within each, the credit that expires soonest goes
  first. A commit's own purchase invoice can never be paid with credit.
  [More](ai-pricing.md#sell-prepaid-credit-a-prepaid-commit)
- **credit note (CN)**: A document that lowers what a customer owes on an
  invoice. On an unpaid invoice it lowers the amount due; if nothing is left
  due, the invoice is marked paid. On a paid one,
  its total is split between a refund to the card, credit to the
  customer's balance, and money returned outside Velox (out of band). Its
  status is draft, issued or voided.
  [More](adr/038-credit-note-three-channel-allocation.md)
- **clawback**: Has two meanings. In subscriptions: a paid period billed in
  advance is cut short (a cancel, a downgrade, a removed item or a plan
  swap). Velox then issues a credit note that adds the unused amount, tax
  included, to the customer's credit balance, and reverses that tax. In the
  credit ledger: a negative entry that removes credit that was granted.
  [More](adr/048-credit-clawback-tax-reversal.md)

## Payments and collection

- **PaymentIntent**: Stripe's name for one attempt to charge a card. Velox
  charges cards only through PaymentIntents, not through Stripe Billing, so
  Velox keeps the invoice. An invoice stores its PaymentIntent ID. A payment
  recorded as made outside Velox has none, and sets `paid_out_of_band`.
  [More](adr/001-paymentintent-only-stripe.md)
- **payment method (PM)**: A customer's saved card. Stripe holds the card,
  and Velox keeps its Stripe ID and the details it shows. A customer can
  have several. Velox marks one as the default and names that exact card on
  every automatic charge, so Stripe never picks one.
  [More](adr/053-explicit-payment-method-at-charge.md)
- **unknown payment status**: A charge got an unclear answer (a timeout, a
  server error, a dropped connection), so Velox cannot tell if money moved.
  Nothing retries it. A Stripe webhook or Velox's regular Stripe check
  settles it. If no PaymentIntent ID was recorded, the invoice is parked.
  [More](adr/107-unknown-is-terminal-until-a-human.md)
- **parked invoice**: An invoice whose charge result is unknown and that has
  no PaymentIntent ID recorded, so Velox cannot yet tell if the customer
  paid. Nothing charges it. On every scheduled billing run (every hour, or
  every 5 minutes with `APP_ENV=local`, but not `POST /v1/billing/run`),
  Velox lists the customer's payments in Stripe. A payment that appears is
  recorded, and if none has appeared
  24 hours after the charge, the invoice is charged again as usual. An
  operator can write it off meanwhile.
  [More](adr/127-a-lost-charge-result-is-resolved-by-asking-stripe.md)
- **parked subscription**: A subscription whose billing failed eight times
  in a row for the same period. Velox stops trying to bill it, so it stays
  due and no invoice is issued for that period. It leaves this state when an
  operator calls `POST /v1/subscriptions/{id}/retry-billing`, or when what
  is due changes. It has nothing to do with a parked invoice.
  [More](adr/119-billing-failure-record-keyed-to-the-due-instant.md)
- **dunning**: Velox's automatic process for collecting an unpaid invoice.
  A failed charge or a missing payment method starts it. It retries the
  charge on a schedule and sends reminders. When the retries run out, the
  dunning policy can pause or cancel the subscription, write off the
  invoice, both, or neither. A new account starts with one policy,
  "Default retries", which retries three times and then does neither.
  [More](how-velox-works.md)
- **dunning run**: One dunning process for one unpaid invoice (at most one
  per invoice). It records the policy, the number of tries, when the next
  action is due, whether it is paused, and its state: active, resolved or
  escalated (every retry failed and the final action was taken). Once
  closed, it also records how it was resolved.
  [More](adr/064-dunning-run-creation-derived-from-invoice-state.md)
- **write-off**: Marking an invoice uncollectible. Velox stops collecting
  it, but keeps it in the records. Nothing charges it again and its tax is
  not reversed, but you can still record a payment made outside Stripe.
  Velox asks Stripe to close a Checkout page still open for it; if Stripe
  refuses, the page stays payable until it expires, at most an hour after
  it opened. If it billed its
  subscription's current period, it still counts as unpaid when that
  subscription changes. An operator, or dunning's final action, writes an
  invoice off.
  [More](adr/110-written-off-invoices-close-self-service-payment.md)

## Costs and margin

- **cost table**: Your list of what an AI provider charges you per token,
  by provider, model and token type, kept separately for live and test
  mode. When a usage event arrives, Velox uses it to record that event's
  cost, unless the sender already sent the real cost.
  [More](ai-pricing.md#see-your-margin)
- **COGS and margin**: COGS (cost of goods sold) is what you pay providers
  for a customer's usage. Velox records it on each usage event when the
  event arrives, from the sent cost or the cost table. Margin is what you
  billed for that usage minus that cost, as a share of what you billed, for
  one customer over a date range. Only you see it, never the customer.
  [More](adr/079-provider-cost-tables.md)
- **cost dashboard**: A view of one customer's usage, charges and expected
  bill for the current period. It never shows your provider cost. It comes
  in two forms: a card on the customer page in the dashboard, and a public,
  read-only JSON address that a partner can embed. That address opens with
  a secret token that is shown once.
  [More](adr/032-public-cost-dashboard-projection.md)

## Testing with simulated time

- **test clock**: A pretend clock for test customers. Create a clock, add a
  new customer on it, then move the clock forward to see renewals, trial
  ends, invoices and payment retries happen in minutes, not months. It
  exists only in test mode. [More](quick-start.md)
- **catch-up (catchup)**: The work Velox does after a test clock moves forward. It
  runs every trial end, billing period, threshold, retry, dunning step and
  credit expiry that fell in the skipped time. It runs them by kind, not in
  time order: trial ends and billing periods first, then payment retries
  and dunning steps, then credit expiries. So when a dunning policy cancels
  or pauses a subscription, move the clock to the time of the invoice's
  last retry first, then to the next billing date. Velox dates the cancel
  or pause at the time the clock moves to, and one longer move bills a
  period a real clock would not
  ([#1276](https://github.com/getvelox/velox/issues/1276)). The clock then
  returns to `ready`, or to `internal_failure` if any step failed.
  [More](adr/015-test-clock-async-catchup.md)
- **clock-pinned**: Describes a customer on a test clock, and everything it
  owns (subscriptions, invoices, credits). Its time comes from the clock,
  not the real clock, and the normal billing run skips it.
  [More](adr/030-simulated-time-everywhere-on-clock-pinned-entities.md)
- **simulated time**: The pretend current time of a test clock. Velox uses
  it for every business date on a clock-pinned record (periods, due dates,
  paid dates). The dashboard marks such invoices and credit notes
  "Simulated", and Home leaves them out of its totals.
  [More](adr/030-simulated-time-everywhere-on-clock-pinned-entities.md)

**Next:** see these words in action in the [quick start](quick-start.md).
