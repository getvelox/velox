# AI pricing reference

**New to Velox?** Start with [Send usage and get your first
invoice](first-invoice.md). It runs every call in order. This page assumes
you did it, so `VELOX`, `VELOX_KEY` and `CUSTOMER_ID` are set in your
terminal.

## Words on this page

| Word | Meaning |
|---|---|
| Dimensions | Labels on a usage event, such as `model` and `token_type`. |
| Rating rule | A price: an amount per unit, tiers, or packages. |
| Pricing rule | A line on a meter that says which events a rating rule prices, by their dimensions. |
| Default price | The price a meter uses for events that no pricing rule picks. |

## Ready-made recipes

A recipe creates a meter, a price for each model and kind of token, and a
monthly plan with no base fee, in one call. It also creates a schedule for
retrying failed card payments, and a webhook endpoint (a URL in your app
that Velox calls when something happens). The endpoint starts turned off,
with a placeholder address: set your own address and turn it on under
**Webhooks**. The prices start at the provider's public list prices, which you can change
later.

| Recipe | Meter | Models it prices | Kinds of token (`token_type`) |
|---|---|---|---|
| `anthropic_style` | `tokens` | `claude-fable-5.1`, `claude-mythos-5.1`, `claude-opus-5.5`, `claude-sonnet-5.5`, `claude-haiku-5.5` (two prices, by `context_tier`), `claude-fable-5`, `claude-mythos-5`, `claude-opus-5`, `claude-sonnet-5`, `claude-opus-4.8`, `claude-opus-4.7`, `claude-opus-4.6`, `claude-opus-4.5`, `claude-opus-4.1`, `claude-opus-4`, `claude-sonnet-4.6`, `claude-sonnet-4.5`, `claude-sonnet-4`, `claude-haiku-4.5`, `claude-3.5-sonnet`, `claude-3.5-haiku`, `claude-3-opus`, `claude-3-sonnet`, `claude-3-haiku` | `input`, `output`, `cache_read`, `cache_write_5m`, `cache_write_1h` |
| `openai_style` | `tokens` | `gpt-5.6-luna`, `gpt-5.6-sol`, `gpt-5.6-terra`, `gpt-5.5`, `gpt-5.5-pro`, `gpt-5.4`, `gpt-5.4-pro`, `gpt-5.4-mini`, `gpt-5.4-nano`, `gpt-5.3-codex`, `gpt-5.1`, `gpt-5`, `gpt-5-mini`, `gpt-5-nano`, `gpt-4.1`, `gpt-4.1-mini`, `gpt-4.1-nano`, `gpt-4o`, `gpt-4o-mini`, `gpt-4-turbo`, `gpt-3.5-turbo`, `o1`, `o1-pro`, `o1-mini`, `o3`, `o3-pro`, `o3-mini`, `o4-mini`, `text-embedding-3-large`, `text-embedding-3-small`, `text-embedding-ada-002` | `input`, `output`, and `cache_read` where the model has it |
| `replicate_style` | `gpu_seconds` | GPU time, by `gpu_type`: `h100`, `a100`, `a40`, `t4`, `cpu` | (none: one price per second for each GPU type) |

Send the model name exactly as the table shows it, in the `model`
dimension. A LiteLLM proxy does this for you: Velox turns a name such as
`claude-sonnet-4-5-20250929` into `claude-sonnet-4.5`.

- **A Claude name matches a model only when it is the model's id, or the id
  with a date or version ending,** such as `-20250929`, `-v1:0`,
  `@20250805` or `@default`. Bedrock, Google Cloud and Foundry prefixes are
  removed first. A name Velox does not know, such as `claude-opus-4-9`, is
  never priced as an older model. It keeps its own name and has no price
  until you add one ([how](#price-a-claude-model-velox-does-not-know)).
- **Claude Haiku 5.5 has two prices.** A call whose prompt is over 100,000
  tokens pays the higher one, for every token of the call, output included.
  The prompt counts input, cache reads and cache writes. Its events carry
  `context_tier`: `prompt_up_to_100k` or `prompt_over_100k`. The recipe has
  a rule for each. If you price Haiku 5.5 yourself, put `context_tier` in
  every rule: a rule without it prices both at one rate. A provider cost
  has no `context_tier`, so your margin on Haiku 5.5 is wrong for one of
  the two prices ([more](#see-your-margin)).
- **The prices are Anthropic's list prices.** On Bedrock and Google Cloud,
  a regional endpoint costs you 10% more than global for Claude 4.5 and
  later models; the recipe charges the same for both. Fast mode, US-only
  inference and batch prices are not applied.

The two AI recipes share the `tokens` meter, so you can apply both. Each
adds its own plan. All prices on one meter must be in one currency, so
apply the second recipe in the currency of the first.

To see the recipes, and to apply one with your own plan name:

```bash
curl -s "$VELOX/v1/recipes" -H "Authorization: Bearer $VELOX_KEY" \
  | jq -c '[.data[] | {key, name}]'
curl -s -X POST "$VELOX/v1/recipes/openai_style/instantiate" \
  -H "Authorization: Bearer $VELOX_KEY" \
  -H "Content-Type: application/json" \
  -d '{"overrides": {"plan_name": "GPT API"}}' \
  | jq -c '{recipe_key, plans: .created_objects.plan_ids}'
```

**You see:** the three recipes, then the recipe you applied and its new
plan:

```text
[{"key":"anthropic_style","name":"Anthropic (Claude) token pricing"},{"key":"openai_style","name":"OpenAI (GPT) token pricing"},{"key":"replicate_style","name":"GPU compute pricing (per-second)"}]
{"recipe_key":"openai_style","plans":["vlx_pln_…"]}
```

The overrides are `currency` (`USD`, `EUR` or `GBP`), `plan_name` and
`plan_code`. Applying a recipe a second time returns the setup it made
the first time, so the overrides count only on the first apply.

## Price a model a recipe does not cover

You need two calls: one makes the price, and one tells the meter which
events it prices.

1. **Make the price.** This is a rating rule. `flat_amount_cents` is the
   price of one unit in cents, and can be a fraction: `0.0002` cents per
   token is $2.00 per million tokens.

   ```bash
   RATE_ID=$(curl -s -X POST "$VELOX/v1/rating-rules" \
     -H "Authorization: Bearer $VELOX_KEY" \
     -H "Content-Type: application/json" \
     -d '{"rule_key": "my_model_input", "name": "My model input", "mode": "flat",
          "currency": "USD", "flat_amount_cents": "0.0002"}' | jq -r .id)
   echo "$RATE_ID"
   ```

   **You see:** the price ID, such as `vlx_rrv_…`.

2. **Find the `tokens` meter.**

   ```bash
   METER_ID=$(curl -s "$VELOX/v1/meters" -H "Authorization: Bearer $VELOX_KEY" \
     | jq -r '.data[] | select(.event_name == "tokens") | .id')
   echo "$METER_ID"
   ```

   **You see:** the meter ID, such as `vlx_mtr_…`.

3. **Add a pricing rule to the meter.** It prices the events whose
   dimensions match `dimension_match`.

   ```bash
   jq -n --arg rate "$RATE_ID" \
       '{dimension_match: {model: "my-model", token_type: "input"},
         rating_rule_version_id: $rate, priority: 100}' \
     | curl -s -X POST "$VELOX/v1/meters/$METER_ID/pricing-rules" \
       -H "Authorization: Bearer $VELOX_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq '{id, dimension_match, priority}'
   ```

   **You see:**

   ```json
   {
     "id": "vlx_mpr_…",
     "dimension_match": {
       "model": "my-model",
       "token_type": "input"
     },
     "priority": 100
   }
   ```

Every plan that charges for the `tokens` meter now prices `my-model` input
tokens. Add one rule for each kind of token you sell.

**While subscriptions use the meter,** Velox accepts a new rule only if it
takes no usage that a price bills now. The meter must have no default
price, and every rule that could match the same events must have a higher
priority. A rule for a new model at priority 100 passes when the other
rules at 100 or below each name a model, as the recipes' rules do.
Otherwise the answer is 409 `meter_in_use`. See [Change a price](#change-a-price).

## How Velox picks the price for an event

- **One price for each event.** Each event is priced by at most one pricing
  rule: the one with the highest priority among the rules that match. So
  nothing is counted twice.
- **One rule for each model and kind of token.** Kinds of token do not
  overlap, so with one rule for each `{model, token_type}`, every event
  matches exactly one rule. This is what the recipes do.
- **A broad rule and finer rules together.** You can add a rule for a whole
  model (`{"model": "my-model"}`) next to rules for each kind of token.
  Priority decides which rule prices each event.

**Usage that no rule picks has no price.** The usual cause is a new model
name. If the meter has a default price, or a broader rule such as
`{"token_type": "input"}` matches it, that price bills it, with no notice. To set one,
send `PATCH /v1/meters/{id}` with `rating_rule_version_id`. If the meter has
no default price, Velox makes and sends the invoice without that usage. You
see it in four places:

- the "not billed" row of the customer's usage, on the customer page and
  in `GET /v1/customers/{id}/usage` (the current billing period, unless
  you pass `from` and `to`);
- the warning on the invoice preview, which gives the quantity;
- the `velox_usage_unpriced_quantity_total{meter_id, mode}` metric (usage
  of customers on a test clock is not counted);
- a server log line when the invoice is made.

To bill it, add a rule or a default price. Invoices not made yet then
include it. If a spend threshold already invoiced the subscription this
period, a sum or count price bills only the usage after that invoice. For a period that is already invoiced, bill it with a one-off
invoice ([runbook](ops/runbook/usage-with-no-price.md)). A first default
price is accepted while subscriptions use the meter. A new rule is accepted while subscriptions use the
meter only as described above.

## Change a price

Usage is priced when its invoice is made, from the meter as it is then. So
while a draft, trialing or active subscription uses the meter, Velox refuses
(409 `meter_in_use`) any change that would reprice the current period:

- editing or deleting a pricing rule;
- a new rule that could take usage from a price that bills it now;
- pointing the default price at another price, or removing it.

The meter's aggregation has its own rule: it can change until the meter has
a usage event, then it is fixed (409 `meter_has_usage`). Held usage does not
count until it is released. Every price on the meter bills under it.

The message says how many subscriptions use the meter. To change a rate,
publish a new version of the rating rule (`POST /v1/rating-rules` with the
same `rule_key`). It applies from each subscription's next billing period,
and the current period keeps the old rate. On the dashboard, that is **New
version** on the Prices tab. A subscription on a test clock is the
exception: a version is stamped with the real time, so if the clock's
current period started later than now, the new version applies to that
period. In test mode the 409 message says so.

## Price a Claude model Velox does not know

A Claude name Velox does not know keeps its own name in both `model` and
`model_raw`. Price it on `model_raw` at priority 101: that value is the
name exactly as LiteLLM sent it, and it does not change when a later
Velox release learns the model. Make one rating rule for each kind of
token, with its own `rule_key`, and add it as in [Price a model a recipe
does not cover](#price-a-model-a-recipe-does-not-cover), with this
pricing rule:

```bash
jq -n --arg rate "$RATE_ID" \
    '{dimension_match: {model_raw: "claude-opus-4-9", token_type: "input"},
      rating_rule_version_id: $rate, priority: 101}' \
  | curl -s -X POST "$VELOX/v1/meters/$METER_ID/pricing-rules" \
    -H "Authorization: Bearer $VELOX_KEY" \
    -H "Content-Type: application/json" \
    -d @- | jq -c '{dimension_match, priority}'
```

**You see:**
`{"dimension_match":{"model_raw":"claude-opus-4-9","token_type":"input"},"priority":101}`

Usage not invoiced yet bills at that price, including usage that arrived
before you added it.

While subscriptions use the meter, Velox refuses this rule (409
`meter_in_use`). At priority 101 it ranks above the model rules of the same
`token_type`, and Velox cannot tell that no event matches both. Then price
the name on `model` instead, at priority 100: `{"model": "claude-opus-4-9",
"token_type": "input"}`. If a later release learns the model and renames it,
that rule stops matching, and the usage shows as not billed until you price
the new name.

## Claude models added after you installed

Applying a recipe again returns the setup it made the first time and adds
nothing. So an install of `anthropic_style` 5.0.0 or earlier does not get
the 6.0.0 prices. After you upgrade Velox:

- Claude Opus 5.5, Sonnet 5.5, Fable 5.1 and Mythos 5.1 usage arrives as
  `claude-opus-5.5`, `claude-sonnet-5.5`, `claude-fable-5.1` and
  `claude-mythos-5.1`. Before, Velox billed it at Opus 5, Sonnet 5, Fable 5
  and Mythos 5 prices. With no rule for the new name, it has no price.
- Claude Haiku 5.5 usage arrives as `claude-haiku-5.5` with `context_tier`.
  Before, it kept its raw name and had no price.
- Claude Sonnet 5 keeps the old $3 / $15 recipe prices until you change
  them. Anthropic now charges $2 / $10.
- A [provider cost](#see-your-margin) saved on `claude-opus-5`,
  `claude-sonnet-5`, `claude-fable-5` or `claude-mythos-5` does not cover
  the new names. Nor does one on `claude-opus-4` or `claude-sonnet-4` cover
  Google Cloud `@default` and `@date` names of Opus 4.1 to 4.8, Sonnet 4.5
  and Sonnet 4.6, which now arrive as their own model, such as
  `claude-opus-4.6`. Save a cost for each new name before you upgrade. A
  cost applies only to usage that arrives after you save it, and while any
  of a customer's usage has no cost, their margin shows no percentage.

Add the 30 prices **before you upgrade**, so no usage arrives without a
price. A rule on `claude-opus-5.5` matches nothing until the new release
sends that name. This script makes them with the recipe's own rule keys,
names and prices:

```bash
CURRENCY=USD   # the currency you installed the recipe in
METER_ID=$(curl -s "$VELOX/v1/meters" -H "Authorization: Bearer $VELOX_KEY" \
  | jq -r '.data[] | select(.event_name == "tokens") | .id')

# add_price <rule key> <name> <cents per token> <model> <token type> [context tier]
add_price() {
  rate_id=$(jq -n --arg key "$1" --arg name "$2" --arg cents "$3" --arg cur "$CURRENCY" \
      '{rule_key: $key, name: $name, mode: "flat", currency: $cur, flat_amount_cents: $cents}' \
    | curl -s -X POST "$VELOX/v1/rating-rules" \
      -H "Authorization: Bearer $VELOX_KEY" -H "Content-Type: application/json" \
      -d @- | jq -r .id)
  jq -n --arg rate "$rate_id" --arg model "$4" --arg type "$5" --arg tier "${6:-}" \
      '{dimension_match: ({model: $model, token_type: $type}
          + (if $tier == "" then {} else {context_tier: $tier} end)),
        rating_rule_version_id: $rate, priority: 100}' \
    | curl -s -X POST "$VELOX/v1/meters/$METER_ID/pricing-rules" \
      -H "Authorization: Bearer $VELOX_KEY" -H "Content-Type: application/json" \
      -d @- | jq -c .dimension_match
}

for m in "fable51|Claude Fable 5.1|claude-fable-5.1|0.001|0.005|0.000025|0.00125|0.002" \
         "mythos51|Claude Mythos 5.1|claude-mythos-5.1|0.001|0.005|0.000025|0.00125|0.002" \
         "opus55|Claude Opus 5.5|claude-opus-5.5|0.0004|0.002|0.00002|0.0005|0.0008" \
         "sonnet55|Claude Sonnet 5.5|claude-sonnet-5.5|0.0002|0.001|0.00001|0.00025|0.0004"; do
  IFS='|' read -r key name model in out read w5m w1h <<<"$m"
  add_price "${key}_input" "$name input" "$in" "$model" input
  add_price "${key}_output" "$name output" "$out" "$model" output
  add_price "${key}_cache_read" "$name cache read" "$read" "$model" cache_read
  add_price "${key}_cache_write_5m" "$name cache write (5m)" "$w5m" "$model" cache_write_5m
  add_price "${key}_cache_write_1h" "$name cache write (1h)" "$w1h" "$model" cache_write_1h
done

for t in "std|prompt_up_to_100k|up to|0.00001|0.00005|0.000001|0.0000125|0.00002" \
         "long|prompt_over_100k|over|0.00005|0.00025|0.000005|0.0000625|0.0001"; do
  IFS='|' read -r key tier words in out read w5m w1h <<<"$t"
  suffix=" (prompts $words 100K tokens)"
  add_price "haiku55_${key}_input" "Claude Haiku 5.5 input$suffix" "$in" claude-haiku-5.5 input "$tier"
  add_price "haiku55_${key}_output" "Claude Haiku 5.5 output$suffix" "$out" claude-haiku-5.5 output "$tier"
  add_price "haiku55_${key}_cache_read" "Claude Haiku 5.5 cache read$suffix" "$read" claude-haiku-5.5 cache_read "$tier"
  add_price "haiku55_${key}_cache_write_5m" "Claude Haiku 5.5 cache write (5m)$suffix" "$w5m" claude-haiku-5.5 cache_write_5m "$tier"
  add_price "haiku55_${key}_cache_write_1h" "Claude Haiku 5.5 cache write (1h)$suffix" "$w1h" claude-haiku-5.5 cache_write_1h "$tier"
done
```

**You see:** 30 lines, the first and last:

```text
{"model":"claude-fable-5.1","token_type":"input"}
…
{"context_tier":"prompt_over_100k","model":"claude-haiku-5.5","token_type":"cache_write_1h"}
```

The prices are cents per token in your recipe's currency, like the
recipe's. Change them to your own prices if you resell at a margin.

**Sonnet 5.** To charge Anthropic's new price, publish a new version of
each Sonnet 5 rule. It applies from each subscription's next billing
period; the current period keeps the old price.

```bash
CURRENCY=USD   # the currency you installed the recipe in

# new_version <rule key> <name> <cents per token>
new_version() {
  jq -n --arg key "$1" --arg name "$2" --arg cents "$3" --arg cur "$CURRENCY" \
      '{rule_key: $key, name: $name, mode: "flat", currency: $cur, flat_amount_cents: $cents}' \
    | curl -s -X POST "$VELOX/v1/rating-rules" \
      -H "Authorization: Bearer $VELOX_KEY" -H "Content-Type: application/json" \
      -d @- | jq -c '{rule_key, version, flat_amount_cents}'
}
new_version sonnet5_input "Claude Sonnet 5 input" 0.0002
new_version sonnet5_output "Claude Sonnet 5 output" 0.001
new_version sonnet5_cache_read "Claude Sonnet 5 cache read" 0.00002
new_version sonnet5_cache_write_5m "Claude Sonnet 5 cache write (5m)" 0.00025
new_version sonnet5_cache_write_1h "Claude Sonnet 5 cache write (1h)" 0.0004
```

**You see:** five lines such as
`{"rule_key":"sonnet5_input","version":2,"flat_amount_cents":"0.0002"}`.

### Rules you wrote on a raw Claude name

Some Claude names used to keep their raw name in `model`, so a rule you
added for one keyed on it. They are Haiku 5.5, Bedrock cross-region names
such as `bedrock/us.anthropic.claude-sonnet-4-6`, `converse/` and
`azure_ai/` names, and some Google Cloud dated names such as
`claude-haiku-4-5@20251001`. Those names now arrive as the model's name,
such as `claude-sonnet-4.6`, so such a rule stops matching.

**Change it before you upgrade:** set its `dimension_match` to `model_raw`
with the raw name, at priority 101. The release you run now also puts the
raw name in `model_raw`, so the rule matches the same usage before and
after the upgrade.

After the upgrade, while a subscription uses the meter, Velox refuses that
change (409 `meter_in_use`). The usage then bills at the price of a rule
on the model's name, if there is one. If there is none and the meter has
no default price, add one with its own rating rule, as in [Price a model a recipe does not
cover](#price-a-model-a-recipe-does-not-cover).

### Usage stored before you upgraded

Velox does not change usage it already stored. Usage stored before the
upgrade keeps the model it was stored with, and bills at that model's
price when its period is invoiced:

- **Google Cloud Opus 4.5 to 4.8** (names with `@default` or `@date`, such
  as `vertex_ai/claude-opus-4-6@default`) were stored as `claude-opus-4`,
  and bill at $15 / $75, 3 times Anthropic's price.
- **Opus 5.5, Sonnet 5.5, Fable 5.1 and Mythos 5.1** were stored as Opus 5,
  Sonnet 5, Fable 5 and Mythos 5, and bill at those prices.

To bill those rows at the right price, **before you upgrade**, add a
rating rule with its own `rule_key`, and a pricing rule on the raw name at
**priority 101**. That priority wins over the recipe's rule; at equal
priority, the older rule wins. Add one for each raw name and kind of
token, such as
`{"model_raw": "vertex_ai/claude-opus-4-6@default", "token_type": "input"}`
at the Opus 4.6 input price. It also prices later calls with that name,
which bill at the same price.

After the upgrade, while a subscription uses the meter, Velox refuses this
rule (409 `meter_in_use`): it takes usage that the recipe's rule bills now.
The period's invoice then bills those rows at the old price. Once it is
made, give the difference back with a credit note (`POST /v1/credit-notes`).

**Usage stored under a raw name that now finds a price** has no price.
These are the names in [Rules you wrote on a raw Claude
name](#rules-you-wrote-on-a-raw-claude-name) other than Haiku 5.5 (below), such as
`vertex_ai/claude-haiku-4-5@20251001` or
`bedrock/us.anthropic.claude-sonnet-4-6`. Velox did not bill those rows,
and still does not; later calls with the same name bill at the model's
price. To bill the rows, add a rule on `model` with the raw name, such as
`{"model": "vertex_ai/claude-haiku-4-5@20251001", "token_type": "input"}`,
at the model's price. Only rows stored before the upgrade carry the raw
name in `model`.

**Haiku 5.5 usage stored before the upgrade** has its raw name in `model`
and no `context_tier`, so the recipe's Haiku 5.5 rules never match it.
Velox cannot tell which of the two prices applies to those rows. To bill
them, add a rule on `model` with the raw name, such as
`{"model": "claude-haiku-5-5", "token_type": "input"}`, at the price you
choose. Only rows stored before the upgrade carry that value in `model`.
Do not use `model_raw` here: it would also price later Haiku 5.5 calls at
one rate.

## Sell prepaid credit (a prepaid commit)

A prepaid commit is a one-off invoice with a special line. When the invoice
is finalized (issued), the customer gets the credit, and later invoices
in the same currency draw it down. Here the customer pays $9,000 for
$10,000 of credit.

1. **Make a one-off invoice with the commit line.** `unit_amount_cents` is
   what the customer pays. `commit_granted_cents` is the credit they get.

   ```bash
   COMMIT_INVOICE_ID=$(jq -n --arg cus "$CUSTOMER_ID" \
       '{customer_id: $cus, line_items: [{description: "Annual commit",
         line_type: "add_on", quantity: 1, unit_amount_cents: 900000,
         commit_granted_cents: 1000000}]}' \
     | curl -s -X POST "$VELOX/v1/invoices" \
       -H "Authorization: Bearer $VELOX_KEY" \
       -H "Content-Type: application/json" \
       -d @- | jq -r .id)
   echo "$COMMIT_INVOICE_ID"
   ```

   **You see:** the invoice ID, such as `vlx_inv_…`. The invoice is a
   draft.

2. **Finalize it.**

   ```bash
   curl -s -X POST "$VELOX/v1/invoices/$COMMIT_INVOICE_ID/finalize" \
     -H "Authorization: Bearer $VELOX_KEY" | jq -c '{status, total_amount_cents}'
   ```

   **You see:** `{"status":"finalized","total_amount_cents":900000}`

3. **Check the credit balance.**

   ```bash
   curl -s "$VELOX/v1/credits/balance/$CUSTOMER_ID" \
     -H "Authorization: Bearer $VELOX_KEY" | jq -c '.data[] | {currency, balance_cents, total_granted}'
   ```

   **You see:** `{"currency":"USD","balance_cents":1000000,"total_granted":1000000}`
   (one line per currency the customer holds credit in).

One invoice can hold at most one commit line. To stop a customer's usage
when the credit runs out, see [Stop usage when credit runs
out](stop-usage-when-credit-runs-out.md).

### Tax on prepaid credit

With a tax provider on, Velox charges tax once: on the usage, not on the
credit purchase. It treats buying credit as an advance payment. The usage the credit pays for is taxed when it is invoiced.

At 10% tax, the commit above is invoiced at $9,000 with no tax. A later
usage invoice of $1,000 carries $100 of tax, and the credit pays all $1,100
of it. So with tax on, the credit covers less usage than its face value:
the $10,000 of credit pays for about $9,091 of usage and $909 of tax.

- **You see:** the commit line has tax 0 and `"tax_reason":
  "prepaid_credit_purchase"`. The dashboard marks it "Prepaid credit, not
  taxed", and the PDF says why there is no tax.
- **Refunds:** refunding unused credit
  ([ADR-080](adr/080-paid-commit-cn-relief.md)) reverses no tax, because the
  purchase carried none. A commit whose purchase was taxed (invoiced before
  [#1288](https://github.com/getvelox/velox/pull/1288)) still reverses that
  tax when refunded.
- **Free (promotional) credit** pays usage invoices the same way, tax
  included.

Tax rules for prepayments differ by country. Some tax a prepayment when it
is received, others when the service is used. Before you sell commits with
tax on, check with your tax adviser:

- whether your country taxes prepaid credit when it is bought or when it is
  used;
- whether that depends on what the credit can be spent on;
- if you sell credit below face value, as in the example, whether its use
  is taxed on the price paid. Velox taxes the usage at its price, so the
  $9,000 commit above leads to tax on about $9,091 of usage;
- what your invoices must say about an untaxed prepayment.

Velox has no setting to tax the purchase instead. If your country requires
it, [open an issue](https://github.com/getvelox/velox/issues) before you
sell commits with tax on. The decision and its reasons are in
[ADR-078](adr/078-prepaid-commits-phase-1.md#amendment-2026-10-19--the-purchase-is-not-taxed-the-usage-it-pays-for-is).

## See your margin

Your margin on a customer is the price of their usage rated in the window,
invoiced or not, against what that usage cost you. Only you see it, never
the customer.

1. **Save what your provider charges you per token.** Do this before the
   usage arrives. Velox records the cost on each event when it arrives, so
   a cost you change later applies only to later events. Save one cost for
   each model and kind of token. These are made-up costs for Claude Sonnet
   4.5: $1.20 per million input tokens and $4.50 per million output tokens.

   `model` must be the name the usage carries exactly, either `model` or
   `model_raw`. A cost on `claude-opus-5` does not cover
   `claude-opus-5.5`.

   A cost has no `context_tier`. So one Claude Haiku 5.5 cost covers both
   of its prices, and the margin is wrong for one of them. At Anthropic's
   list prices, a prompt over 100,000 tokens costs 5 times as much: if you
   save the cost of shorter prompts, the margin on longer ones reads too
   high. When a LiteLLM record has `cost_breakdown`, Velox can use
   LiteLLM's cost instead of yours
   ([when](integrations/litellm.md#things-to-know)).

   ```bash
   save_cost() {   # save_cost <token type> <cost per token in dollars>
     jq -n --arg type "$1" --arg cost "$2" \
         '{provider: "anthropic", model: "claude-sonnet-4.5",
           token_type: $type, cost_per_token: $cost}' \
       | curl -s -X PUT "$VELOX/v1/provider-costs" \
         -H "Authorization: Bearer $VELOX_KEY" \
         -H "Content-Type: application/json" \
         -d @- | jq -c '{id, token_type, cost_per_token}'
   }
   save_cost input 0.0000012
   save_cost output 0.0000045
   ```

   **You see:**

   ```text
   {"id":"vlx_pcr_…","token_type":"input","cost_per_token":"0.0000012"}
   {"id":"vlx_pcr_…","token_type":"output","cost_per_token":"0.0000045"}
   ```

   The **Provider costs** page lists them.

2. **Read the margin for one invoiced period.** This reads the billing
   period of the invoice in `INVOICE_ID`.

   ```bash
   PERIOD=$(curl -s "$VELOX/v1/invoices/$INVOICE_ID" \
     -H "Authorization: Bearer $VELOX_KEY" \
     | jq -r '.invoice | "from=\(.billing_period_start | @uri)&to=\(.billing_period_end | @uri)"')
   curl -s "$VELOX/v1/customers/$CUSTOMER_ID/margin?$PERIOD" \
     -H "Authorization: Bearer $VELOX_KEY" | jq '{revenue_cents, cost_micros, margin_bps}'
   ```

   **You see,** for a [first invoice](first-invoice.md) made after you
   saved these two costs:

   ```json
   {
     "revenue_cents": 600,
     "cost_micros": 2100000,
     "margin_bps": 6500
   }
   ```

   Billed $6.00, cost $2.10 (`cost_micros` is in millionths of a dollar),
   and a 65% margin (`margin_bps` is in hundredths of a percent).

The margin covers usage only: base fees, credits and taxes are left out.
Revenue Velox cannot match to a model is shown apart, in
`unattributed_revenue_cents`. A token event that names its `provider`,
`token_type` and `model` but matches no cost rate is counted in
`unresolved_events`. So is an event whose cost, quantity ×
`cost_per_token`, is too large to record (about $9.2 trillion or more);
it is still billed. While any such event is in the window,
`margin_bps` is left out. An event sent through the usage API without one
of those three labels, such as usage on a meter that is not for tokens,
gets no cost and is not counted. Its price is still in `revenue_cents`, so
`margin_bps` reads higher than your real margin.

Without `from` and `to`, the margin covers the last 30 days of the
customer's time. On a test clock, that is the clock's time. The
**Margin** card on the customer page starts on the current billing period.

## Usage from a LiteLLM proxy

A LiteLLM proxy can send each AI call to Velox as token events on the
`tokens` meter. Setup, the `user` field, which calls are billed and how
cache tokens are billed: [LiteLLM integration](integrations/litellm.md).

Next: [Stop usage when credit runs out](stop-usage-when-credit-runs-out.md).
