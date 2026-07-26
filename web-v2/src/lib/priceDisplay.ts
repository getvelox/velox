// Shared price-display helpers (extracted from the recipe preview, PR #608;
// promoted for the whole Pricing section, ADR-100 follow-up).
//
// Rates are exact decimal-string cents per unit. Display does decimal-point
// arithmetic on the STRING — never parseFloat — so sub-cent rates render
// exactly ("0.0000275" ×10⁴ → "0.275", never 0.27500000000000002), and a
// rate is never rounded: $0.125 stays $0.125. Whole-dollar values pad to
// money-standard two decimals ($5.00).
//
// Unit convention: token rates quote per 1M tokens (how every AI provider
// and rate card lists them); everything else quotes per single unit
// ("$0.0014 / second"). Currency comes from the ENTITY (rule/plan), never
// the tenant's active display default — post-ADR-100 every catalog object
// carries its own pinned currency.

import { getCurrencySymbol } from './api'

// PricedRule is the structural shape both the hand-rolled catalog RatingRule
// and the generated RecipeRatingRule satisfy.
export interface PricedRule {
  mode: string
  currency?: string
  flat_amount_cents?: string
  graduated_tiers?: { up_to?: number; unit_amount_cents?: string }[]
  package_size?: number
  package_amount_cents?: number
  overage_unit_amount_cents?: string
}

// shiftDecimal moves a decimal string's point right by `places` without
// float math.
export function shiftDecimal(s: string, places: number): string {
  const neg = s.startsWith('-')
  const [rawInt, rawFrac = ''] = (neg ? s.slice(1) : s).split('.')
  const digits = rawInt + rawFrac
  const point = rawInt.length + places
  let out: string
  if (point <= 0) {
    out = '0.' + '0'.repeat(-point) + digits
  } else if (point >= digits.length) {
    out = digits + '0'.repeat(point - digits.length)
  } else {
    out = digits.slice(0, point) + '.' + digits.slice(point)
  }
  out = out.replace(/^0+(?=\d)/, '')
  if (out.includes('.')) out = out.replace(/\.?0+$/, '')
  return (neg ? '-' : '') + (out === '' ? '0' : out)
}

// money renders a cents-decimal-string scaled by 10^shift with the entity's
// currency symbol — padded to two decimals, never rounded.
export function money(cents: string, shift: number, currency?: string): string {
  let v = shiftDecimal(cents, shift - 2)
  const frac = v.split('.')[1] ?? ''
  if (frac.length < 2) v = (frac.length === 0 ? v + '.' : v) + '0'.repeat(2 - frac.length)
  return getCurrencySymbol(currency) + v
}

// singular trims a plural unit for "per <unit>" copy (seconds → second).
export function singular(unit: string): string {
  return unit.length > 1 && unit.endsWith('s') ? unit.slice(0, -1) : unit
}

// unitScale gives the display scale for a meter unit: tokens per 1M,
// anything else per single unit.
export function unitScale(unit: string): { scale: number; per: string } {
  // Meters in the wild carry both 'tokens' and 'token' — same semantics.
  const perMillion = unit.trim().toLowerCase() === 'tokens' || unit.trim().toLowerCase() === 'token'
  return {
    scale: perMillion ? 6 : 0,
    per: perMillion ? '1M tokens' : singular(unit || 'unit'),
  }
}

// priceRate renders one rule's money in operator units, across all three
// pricing modes, in the rule's own currency. `unit` is the meter's unit
// when known ('' falls back to "per unit").
export function priceRate(r: PricedRule, unit: string): string {
  const { scale, per } = unitScale(unit)
  const cur = r.currency
  switch (r.mode) {
    case 'graduated': {
      const tiers = r.graduated_tiers ?? []
      if (tiers.length === 0) return `tiered / ${per}`
      const parts = tiers.map(t =>
        t.up_to && t.up_to > 0
          ? `up to ${t.up_to.toLocaleString()} @ ${money(t.unit_amount_cents ?? '0', scale, cur)}`
          : `beyond @ ${money(t.unit_amount_cents ?? '0', scale, cur)}`,
      )
      return `tiered / ${per}: ${parts.join(', ')}`
    }
    case 'package': {
      const pkg = `${money(String(r.package_amount_cents ?? 0), 0, cur)} per ${(r.package_size ?? 0).toLocaleString()} ${unit || 'units'}`
      const overage = r.overage_unit_amount_cents && r.overage_unit_amount_cents !== '0'
        ? `, then ${money(r.overage_unit_amount_cents, scale, cur)} / ${per}`
        : ''
      return pkg + overage
    }
    default:
      return `${money(r.flat_amount_cents ?? '0', scale, cur)} / ${per}`
  }
}
