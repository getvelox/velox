import { test, expect, type Page, type Response } from '@playwright/test'
import { api, usd, uniq } from './fixtures/api'
import {
  createClock, createCustomer, createPlan, createSubscription, invoicesFor, subscribedCustomer, waitForClockReady,
  type Invoice, type Plan, type Subscription, type TestClock,
} from './fixtures/seed'

// Changing a subscription through the screen: its plan, its items, its end,
// its collection and its trial. Each journey does what an operator does, then
// reads the API and checks that the page, the toast and any invoice the change
// made show what the API holds. One test per kind of change. The arithmetic
// behind the amounts is tested in Go.

const DAY = 86_400_000

// The subscription fields these journeys read, beyond the shared fixture's.
interface Item {
  id: string
  plan_id: string
  quantity: number
  pending_plan_id?: string
  pending_plan_effective_at?: string
}
interface Sub extends Subscription {
  items: Item[]
  trial_start_at?: string
  trial_end_at?: string
  current_billing_period_end?: string
  next_billing_at?: string
  cancel_at_period_end?: boolean
  cancel_effective_at?: string
  canceled_at?: string
  pause_collection?: { behavior: string; resumes_at?: string }
}
interface ItemChange {
  item: Item
  proration?: { type: 'invoice' | 'credit' | 'adjustment'; amount_cents: number; invoice_id?: string }
}
interface Preview {
  lines: { description: string; amount_cents: number }[]
  totals: { currency: string; amount_cents: number }[]
}

function getSub(id: string) {
  return api<Sub>('GET', `/subscriptions/${id}`)
}

async function subscriptionInvoices(id: string): Promise<Invoice[]> {
  return (await api<{ data: Invoice[] }>('GET', `/invoices?subscription_id=${id}`)).data
}

// A monthly USD plan whose base fee is billed at the start of each period, so
// a change in the middle of one is charged or credited at once.
function planBilledInAdvance(baseCents: number): Promise<Plan> {
  const id = uniq()
  return api<Plan>('POST', '/plans', {
    code: `e2e-adv-${id}`,
    name: `E2E Advance ${id}`,
    currency: 'USD',
    billing_interval: 'monthly',
    base_amount_cents: baseCents,
    base_bill_timing: 'in_advance',
  })
}

async function advanceDays(clock: TestClock, days: number) {
  const to = new Date(new Date(clock.frozen_time).getTime() + days * DAY)
  await api('POST', `/test-clocks/${clock.id}/advance`, { frozen_time: to.toISOString().replace(/\.\d+Z$/, 'Z') })
  await waitForClockReady(clock.id)
}

// The workspace time zone: the dashboard prints every date in it.
async function workspaceZone(): Promise<string> {
  return (await api<{ timezone?: string }>('GET', '/settings')).timezone || 'UTC'
}

// A calendar date as the dashboard prints it: "Nov 18, 2026".
function day(iso: string, timeZone: string): string {
  return new Date(iso).toLocaleDateString('en-US', { year: 'numeric', month: 'short', day: 'numeric', timeZone })
}

// The same date as typed into a date field: "2026-11-18".
function typedDate(at: Date, timeZone: string): string {
  return new Intl.DateTimeFormat('en-CA', { year: 'numeric', month: '2-digit', day: '2-digit', timeZone }).format(at)
}

function escapeRegExp(s: string): string {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
}

// The next API call the page makes with this method to a path ending in `path`.
function apiCall(page: Page, method: string, path: RegExp): Promise<Response> {
  return page.waitForResponse(r => r.request().method() === method && path.test(new URL(r.url()).pathname))
}

// Toasts render outside the page's main region.
function toasts(page: Page) {
  return page.getByRole('region', { name: /^Notifications/ })
}

function card(page: Page, title: string | RegExp) {
  return page.getByRole('main').locator('[data-slot="card"]').filter({ has: page.getByText(title, { exact: true }) })
}

function itemsCard(page: Page) {
  return page.getByRole('main').locator('[data-slot="card"]').filter({ has: page.getByRole('columnheader', { name: 'Pending change' }) })
}

// The "Billing cycle" (or "Trial") cell of the summary card and its value.
function summaryCell(page: Page, label: string) {
  return page.getByRole('main').getByText(label, { exact: true }).locator('..')
}

// The date the summary card gives for the next renewal: the API's next
// billing date.
function renewsOn(sub: Sub, zone: string): string {
  return day(sub.next_billing_at ?? sub.current_billing_period_end!, zone)
}

// The Upcoming invoice card lists the lines the API previews for the
// subscription, in order, with their amounts and the total.
async function expectUpcomingInvoiceMatchesAPI(page: Page, subscriptionId: string) {
  const preview = await api<Preview>('GET', `/billing/preview/${subscriptionId}`)
  expect(preview.totals).toHaveLength(1)
  const upcoming = card(page, 'Upcoming invoice')
  await expect(upcoming.getByRole('row')).toHaveText([
    /^Description\s*Quantity\s*Amount$/,
    ...preview.lines.map(l => new RegExp(`^${escapeRegExp(l.description)}.*${escapeRegExp(usd(l.amount_cents))}$`)),
  ])
  await expect(upcoming).toContainText(usd(preview.totals[0].amount_cents))
}

test('change plan now: the toast and the proration invoice match the API; a change at renewal shows as pending until canceled', async ({ page }) => {
  const small = await planBilledInAdvance(4900)
  const bigger = await planBilledInAdvance(9900)
  const clock = await createClock()
  const customer = await createCustomer(clock.id)
  const subscription = await createSubscription(customer.id, small.id)
  // The first period is invoiced when the subscription starts. Record that
  // invoice as paid: an upgrade on an unpaid period is refused rather than
  // charged twice (ADR-050), and this journey is about the charge.
  const started = await invoicesFor(customer.id)
  expect(started).toHaveLength(1)
  await api('POST', `/invoices/${started[0].id}/record-payment`, { note: 'E2E paid outside Stripe' })
  const zone = await workspaceZone()

  await page.goto(`/subscriptions/${subscription.id}`)
  const items = itemsCard(page)
  const dialog = page.getByRole('dialog', { name: 'Change plan' })

  // Change now: the item moves to the bigger plan and the difference is
  // charged on a proration invoice, which the toast names.
  await items.getByRole('button', { name: 'Change plan', exact: true }).click()
  await dialog.getByRole('button', { name: 'Select a plan...' }).click()
  await page.getByRole('option', { name: new RegExp(bigger.code) }).click()
  await dialog.getByRole('checkbox', { name: /^Change now/ }).check()
  const changingNow = apiCall(page, 'PATCH', /\/items\/[^/]+$/)
  await dialog.getByRole('button', { name: 'Change plan', exact: true }).click()
  const nowRes = await changingNow
  expect(nowRes.ok()).toBe(true)
  const now = (await nowRes.json()) as ItemChange
  expect(now.proration?.type).toBe('invoice')
  const proration = now.proration!
  await expect(toasts(page).getByText(`Proration invoice created for ${usd(proration.amount_cents)}`, { exact: true })).toBeVisible()
  await expect(dialog).toBeHidden()

  expect((await getSub(subscription.id)).items.map(i => i.plan_id)).toEqual([bigger.id])
  await expect(items.getByRole('row').filter({ hasText: bigger.name })).toBeVisible()
  await expect(items.getByRole('row').filter({ hasText: small.name })).toHaveCount(0)
  // The e2e workspace charges no tax, so the invoice totals the proration.
  const { invoice } = await api<{ invoice: Invoice }>('GET', `/invoices/${proration.invoice_id}`)
  expect([invoice.subtotal_cents, invoice.total_amount_cents]).toEqual([proration.amount_cents, proration.amount_cents])
  await expect(card(page, /^Invoices \(\d+\)$/).getByRole('row').filter({ hasText: invoice.invoice_number })).toContainText(usd(invoice.total_amount_cents))

  // Change at renewal (Change now left unticked): nothing is charged; the
  // item shows the pending plan and its date until the change is canceled.
  await items.getByRole('button', { name: 'Change plan', exact: true }).click()
  await dialog.getByRole('button', { name: 'Select a plan...' }).click()
  await page.getByRole('option', { name: new RegExp(small.code) }).click()
  await expect(dialog.getByRole('checkbox', { name: /^Change now/ })).not.toBeChecked()
  const scheduling = apiCall(page, 'PATCH', /\/items\/[^/]+$/)
  await dialog.getByRole('button', { name: 'Change plan', exact: true }).click()
  const laterRes = await scheduling
  expect(laterRes.ok()).toBe(true)
  const later = (await laterRes.json()) as ItemChange
  expect(later.proration).toBeUndefined()
  expect(later.item.pending_plan_id).toBe(small.id)
  const effectiveOn = day(later.item.pending_plan_effective_at!, zone)
  await expect(toasts(page).getByText(`The new plan starts ${effectiveOn}.`, { exact: true })).toBeVisible()

  const pending = (await getSub(subscription.id)).items
  expect(pending.map(i => [i.plan_id, i.pending_plan_id])).toEqual([[bigger.id, small.id]])
  expect(day(pending[0].pending_plan_effective_at!, zone)).toBe(effectiveOn)
  expect(await subscriptionInvoices(subscription.id)).toHaveLength(2)
  const row = items.getByRole('row').filter({ hasText: bigger.name })
  await expect(row.getByText(`→ ${small.name}`, { exact: true })).toBeVisible()
  await expect(row).toContainText(effectiveOn)

  await row.getByRole('button', { name: 'Cancel', exact: true }).click()
  await expect(toasts(page).getByText('Pending plan change canceled', { exact: true })).toBeVisible()
  const cleared = (await getSub(subscription.id)).items
  expect(cleared.map(i => [i.plan_id, i.pending_plan_id ?? null, i.pending_plan_effective_at ?? null])).toEqual([[bigger.id, null, null]])
  await expect(row).not.toContainText(small.name)
  await expect(row.getByRole('button', { name: 'Cancel', exact: true })).toHaveCount(0)
})

test('cancel at period end shows its date and can be undone; cancel now needs CANCEL typed, reads "Ended" and bills the days used', async ({ page }) => {
  const { clock, subscription } = await subscribedCustomer()
  // Ten days into the period, so a cancel now has days to bill.
  await advanceDays(clock, 10)
  const zone = await workspaceZone()
  const before = await getSub(subscription.id)

  await page.goto(`/subscriptions/${subscription.id}`)
  const main = page.getByRole('main')
  const billingCycle = summaryCell(page, 'Billing cycle')
  await expect(billingCycle).toContainText(`Renews ${renewsOn(before, zone)}`)
  const choice = page.getByRole('alertdialog', { name: 'Cancel subscription' })

  // At period end: the API keeps it active with a cancel date, and the page
  // names that date until Undo clears it.
  await main.getByRole('button', { name: 'Cancel', exact: true }).click()
  await expect(choice.getByRole('radio', { name: /^At period end/ })).toBeChecked()
  const scheduling = apiCall(page, 'POST', /\/schedule-cancel$/)
  await choice.getByRole('button', { name: 'Schedule cancellation' }).click()
  expect((await scheduling).ok()).toBe(true)
  const scheduled = await getSub(subscription.id)
  expect([scheduled.status, scheduled.cancel_at_period_end]).toEqual(['active', true])
  const cancelsOn = day(scheduled.cancel_effective_at!, zone)
  await expect(toasts(page).getByText(`Cancels at the end of the current period, ${cancelsOn}.`, { exact: true })).toBeVisible()
  await expect(main.getByText(`it doesn't renew on ${cancelsOn}.`, { exact: false })).toBeVisible()
  await expect(billingCycle).toContainText(`Cancels ${cancelsOn}`)

  const undoing = apiCall(page, 'DELETE', /\/scheduled-cancel$/)
  await main.getByRole('button', { name: 'Undo', exact: true }).click()
  expect((await undoing).ok()).toBe(true)
  await expect(toasts(page).getByText('Cancellation undone', { exact: true })).toBeVisible()
  const undone = await getSub(subscription.id)
  expect([undone.status, undone.cancel_at_period_end ?? false, undone.cancel_effective_at ?? null]).toEqual(['active', false, null])
  // The notice is gone (the Activity list keeps its "Cancellation scheduled"
  // entry, so look for the notice's own words and button).
  await expect(main.getByText(`it doesn't renew on ${cancelsOn}.`, { exact: false })).toHaveCount(0)
  await expect(main.getByRole('button', { name: 'Undo', exact: true })).toHaveCount(0)
  await expect(billingCycle).toContainText(`Renews ${renewsOn(undone, zone)}`)

  // Immediately: the typed confirm stays disabled until CANCEL is typed.
  await main.getByRole('button', { name: 'Cancel', exact: true }).click()
  await choice.getByRole('radio', { name: /^Immediately/ }).click()
  await choice.getByRole('button', { name: 'Cancel immediately…' }).click()
  const confirm = page.getByRole('alertdialog', { name: 'Cancel subscription now' })
  const cancelNow = confirm.getByRole('button', { name: 'Cancel subscription now' })
  await expect(cancelNow).toBeDisabled()
  await confirm.getByLabel('Type CANCEL to confirm').fill('CANCE')
  await expect(cancelNow).toBeDisabled()
  await confirm.getByLabel('Type CANCEL to confirm').fill('CANCEL')
  const canceling = apiCall(page, 'POST', /\/cancel$/)
  await cancelNow.click()
  expect((await canceling).ok()).toBe(true)
  await expect(toasts(page).getByText('Subscription canceled', { exact: true })).toBeVisible()

  const canceled = await getSub(subscription.id)
  expect(canceled.status).toBe('canceled')
  const endedOn = day(canceled.canceled_at!, zone)
  await expect(billingCycle).toContainText(`Canceled ${endedOn}`)
  await expect(main.getByText(/Renews/)).toHaveCount(0)
  // The ten days used are billed on a final invoice, the one the page lists.
  const invoices = await subscriptionInvoices(subscription.id)
  expect(invoices).toHaveLength(1)
  expect(invoices[0].total_amount_cents).toBeGreaterThan(0)
  await expect(card(page, 'Invoices (1)').getByRole('row').filter({ hasText: invoices[0].invoice_number })).toContainText(usd(invoices[0].total_amount_cents))

  await page.goto(`/subscriptions?search=${encodeURIComponent(subscription.code)}`)
  await expect(page.getByRole('row').filter({ hasText: subscription.display_name })).toContainText(`Ended ${endedOn}`)
})

test('change quantity, add an item, remove it: the upcoming invoice shows what the API previews after each change', async ({ page }) => {
  const { clock, plan, subscription } = await subscribedCustomer()
  const extra = await createPlan()
  // Ten days in, so each change splits the period.
  await advanceDays(clock, 10)

  await page.goto(`/subscriptions/${subscription.id}`)
  const items = itemsCard(page)

  // Quantity 1 → 3. The plan bills at the period end, so nothing is charged
  // now and the toast says only that the quantity changed.
  await items.getByRole('button', { name: 'Change quantity' }).click()
  const quantity = page.getByRole('dialog', { name: 'Change quantity' })
  await quantity.getByLabel('Quantity').fill('3')
  const changing = apiCall(page, 'PATCH', /\/items\/[^/]+$/)
  await quantity.getByRole('button', { name: 'Save' }).click()
  const changeRes = await changing
  expect(changeRes.ok()).toBe(true)
  expect(((await changeRes.json()) as ItemChange).proration).toBeUndefined()
  await expect(toasts(page).getByText('Quantity updated', { exact: true })).toBeVisible()
  expect((await getSub(subscription.id)).items.map(i => [i.plan_id, i.quantity])).toEqual([[plan.id, 3]])
  await expect(items.getByRole('row').filter({ hasText: plan.name }).getByRole('cell').nth(1)).toHaveText('3')
  await expectUpcomingInvoiceMatchesAPI(page, subscription.id)

  // Add a second plan.
  await items.getByRole('button', { name: 'Add item' }).click()
  const add = page.getByRole('dialog', { name: 'Add item' })
  await add.getByRole('button', { name: 'Select a plan...' }).click()
  await page.getByRole('option', { name: new RegExp(extra.code) }).click()
  const adding = apiCall(page, 'POST', /\/items$/)
  await add.getByRole('button', { name: 'Add item' }).click()
  expect((await adding).ok()).toBe(true)
  await expect(toasts(page).getByText('Item added', { exact: true })).toBeVisible()
  const two = (await getSub(subscription.id)).items
  expect(two.map(i => [i.plan_id, i.quantity]).sort()).toEqual([[plan.id, 3], [extra.id, 1]].sort())
  await expect(items.getByRole('row').filter({ hasText: extra.name })).toBeVisible()
  await expectUpcomingInvoiceMatchesAPI(page, subscription.id)

  // Remove it again.
  await items.getByRole('row').filter({ hasText: extra.name }).getByRole('button', { name: 'Remove' }).click()
  const removing = apiCall(page, 'DELETE', /\/items\/[^/]+$/)
  await page.getByRole('alertdialog', { name: 'Remove item from subscription?' }).getByRole('button', { name: 'Remove item' }).click()
  expect((await removing).ok()).toBe(true)
  await expect(toasts(page).getByText('Item removed', { exact: true })).toBeVisible()
  expect((await getSub(subscription.id)).items.map(i => i.plan_id)).toEqual([plan.id])
  await expect(items.getByRole('row').filter({ hasText: extra.name })).toHaveCount(0)
  await expectUpcomingInvoiceMatchesAPI(page, subscription.id)
})

test('extend a trial, end it now (the first period is invoiced), then pause collection with an auto-resume date and resume it', async ({ page }) => {
  const plan = await planBilledInAdvance(4900)
  const clock = await createClock()
  const customer = await createCustomer(clock.id)
  const id = uniq()
  const subscription = await api<Sub>('POST', '/subscriptions', {
    code: `e2e-trial-${id}`,
    display_name: `E2E Trial ${id}`,
    customer_id: customer.id,
    items: [{ plan_id: plan.id, quantity: 1 }],
    trial_days: 14,
    start_now: true,
  })
  expect(subscription.status).toBe('trialing')
  const zone = await workspaceZone()

  await page.goto(`/subscriptions/${subscription.id}`)
  const main = page.getByRole('main')
  await expect(summaryCell(page, 'Trial')).toContainText(`14-day trial · ends ${day(subscription.trial_end_at!, zone)}`)

  // Extend: the dialog offers a week past the current trial end.
  await main.getByRole('button', { name: 'Extend trial' }).click()
  const extending = apiCall(page, 'POST', /\/extend-trial$/)
  await page.getByRole('dialog', { name: 'Extend trial' }).getByRole('button', { name: 'Extend', exact: true }).click()
  expect((await extending).ok()).toBe(true)
  const extended = await getSub(subscription.id)
  const endsOn = day(extended.trial_end_at!, zone)
  await expect(toasts(page).getByText(`The trial now ends ${endsOn}`, { exact: false })).toBeVisible()
  expect(extended.status).toBe('trialing')
  expect(Math.round((Date.parse(extended.trial_end_at!) - Date.parse(subscription.trial_end_at!)) / DAY)).toBe(7)
  await expect(summaryCell(page, 'Trial')).toContainText(`21-day trial · ends ${endsOn}`)

  // End it now: the subscription is active and, billed in advance, its first
  // period is invoiced at once; the page lists that invoice.
  const ending = apiCall(page, 'POST', /\/end-trial$/)
  await main.getByRole('button', { name: 'End trial now' }).click()
  expect((await ending).ok()).toBe(true)
  await expect(toasts(page).getByText('Trial ended — subscription is now active', { exact: true })).toBeVisible()
  const active = await getSub(subscription.id)
  expect(active.status).toBe('active')
  await expect(summaryCell(page, 'Billing cycle')).toContainText(`Renews ${renewsOn(active, zone)}`)
  await expect(main.getByRole('button', { name: 'End trial now' })).toHaveCount(0)
  const invoices = await subscriptionInvoices(subscription.id)
  expect(invoices).toHaveLength(1)
  await expect(card(page, 'Invoices (1)').getByRole('row').filter({ hasText: invoices[0].invoice_number })).toContainText(usd(invoices[0].total_amount_cents))

  // Pause collection until a date ten days out: the API keeps new invoices
  // as drafts until then, and the page and toast name that day.
  const resumeAt = new Date(new Date(clock.frozen_time).getTime() + 10 * DAY)
  await main.getByRole('button', { name: 'Pause collection…' }).click()
  const pause = page.getByRole('dialog', { name: 'Pause collection' })
  await pause.getByPlaceholder('Pick a date').fill(typedDate(resumeAt, zone))
  await pause.getByPlaceholder('Pick a date').press('Enter')
  const pausing = apiCall(page, 'PUT', /\/pause-collection$/)
  await pause.getByRole('button', { name: 'Pause collection', exact: true }).click()
  expect((await pausing).ok()).toBe(true)
  const paused = await getSub(subscription.id)
  expect(paused.pause_collection?.behavior).toBe('keep_as_draft')
  const resumesOn = day(paused.pause_collection!.resumes_at!, zone)
  expect(resumesOn).toBe(day(resumeAt.toISOString(), zone))
  await expect(toasts(page).getByText(`New invoices are saved as drafts until ${resumesOn}.`, { exact: true })).toBeVisible()
  await expect(main.getByText(`Collection resumes ${resumesOn}`, { exact: false })).toBeVisible()
  await expect(main.getByRole('button', { name: 'Pause collection…' })).toHaveCount(0)

  const resuming = apiCall(page, 'DELETE', /\/pause-collection$/)
  await main.getByRole('button', { name: 'Resume collection' }).click()
  expect((await resuming).ok()).toBe(true)
  await expect(toasts(page).getByText('Collection resumed', { exact: true })).toBeVisible()
  expect((await getSub(subscription.id)).pause_collection ?? null).toBeNull()
  // The notice and the timeline's resume date are gone (the Activity list
  // keeps its "Collection paused" entry).
  await expect(main.getByRole('button', { name: 'Resume collection' })).toHaveCount(0)
  await expect(main.getByText(/Collection resumes/)).toHaveCount(0)
  await expect(main.getByRole('button', { name: 'Pause collection…' })).toBeVisible()
})
