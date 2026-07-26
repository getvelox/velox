// Shared geo/locale constants used by both tenant Settings and per-customer
// Billing Profile forms. Single source of truth for countries, states,
// currencies, and timezones so the two sides of the app stay in sync.
//
// Country codes are ISO-3166 alpha-2. Currency codes are ISO 4217.
// Timezones are sourced from @vvo/tzdb (industry-standard IANA metadata).

import { getTimeZones, type TimeZone } from '@vvo/tzdb'

export type CountryCode = string
export type CurrencyCode = string

// Common countries sorted by rough billing-volume priority. Full ISO-3166
// would be 250+ entries; this list covers >99% of SaaS billing traffic.
// Consumers render via <Combobox> so the list is searchable regardless
// of length.
export const COUNTRIES: ReadonlyArray<readonly [CountryCode, string]> = [
  ['US', 'United States'],
  ['CA', 'Canada'],
  ['GB', 'United Kingdom'],
  ['IE', 'Ireland'],
  ['DE', 'Germany'],
  ['FR', 'France'],
  ['ES', 'Spain'],
  ['IT', 'Italy'],
  ['NL', 'Netherlands'],
  ['BE', 'Belgium'],
  ['SE', 'Sweden'],
  ['NO', 'Norway'],
  ['DK', 'Denmark'],
  ['FI', 'Finland'],
  ['PL', 'Poland'],
  ['PT', 'Portugal'],
  ['CH', 'Switzerland'],
  ['AT', 'Austria'],
  ['CZ', 'Czechia'],
  ['GR', 'Greece'],
  ['IN', 'India'],
  ['JP', 'Japan'],
  ['KR', 'South Korea'],
  ['CN', 'China'],
  ['HK', 'Hong Kong'],
  ['TW', 'Taiwan'],
  ['SG', 'Singapore'],
  ['MY', 'Malaysia'],
  ['TH', 'Thailand'],
  ['PH', 'Philippines'],
  ['ID', 'Indonesia'],
  ['VN', 'Vietnam'],
  ['AU', 'Australia'],
  ['NZ', 'New Zealand'],
  ['AE', 'United Arab Emirates'],
  ['SA', 'Saudi Arabia'],
  ['IL', 'Israel'],
  ['TR', 'Turkey'],
  ['ZA', 'South Africa'],
  ['NG', 'Nigeria'],
  ['KE', 'Kenya'],
  ['EG', 'Egypt'],
  ['BR', 'Brazil'],
  ['MX', 'Mexico'],
  ['AR', 'Argentina'],
  ['CL', 'Chile'],
  ['CO', 'Colombia'],
  ['PE', 'Peru'],
]

export const COUNTRY_NAME: Record<string, string> = Object.fromEntries(COUNTRIES)

export const US_STATES: ReadonlyArray<readonly [string, string]> = [
  ['AL', 'Alabama'], ['AK', 'Alaska'], ['AZ', 'Arizona'], ['AR', 'Arkansas'], ['CA', 'California'],
  ['CO', 'Colorado'], ['CT', 'Connecticut'], ['DE', 'Delaware'], ['DC', 'District of Columbia'], ['FL', 'Florida'],
  ['GA', 'Georgia'], ['HI', 'Hawaii'], ['ID', 'Idaho'], ['IL', 'Illinois'], ['IN', 'Indiana'],
  ['IA', 'Iowa'], ['KS', 'Kansas'], ['KY', 'Kentucky'], ['LA', 'Louisiana'], ['ME', 'Maine'],
  ['MD', 'Maryland'], ['MA', 'Massachusetts'], ['MI', 'Michigan'], ['MN', 'Minnesota'], ['MS', 'Mississippi'],
  ['MO', 'Missouri'], ['MT', 'Montana'], ['NE', 'Nebraska'], ['NV', 'Nevada'], ['NH', 'New Hampshire'],
  ['NJ', 'New Jersey'], ['NM', 'New Mexico'], ['NY', 'New York'], ['NC', 'North Carolina'], ['ND', 'North Dakota'],
  ['OH', 'Ohio'], ['OK', 'Oklahoma'], ['OR', 'Oregon'], ['PA', 'Pennsylvania'], ['RI', 'Rhode Island'],
  ['SC', 'South Carolina'], ['SD', 'South Dakota'], ['TN', 'Tennessee'], ['TX', 'Texas'], ['UT', 'Utah'],
  ['VT', 'Vermont'], ['VA', 'Virginia'], ['WA', 'Washington'], ['WV', 'West Virginia'], ['WI', 'Wisconsin'], ['WY', 'Wyoming'],
]

export const CA_PROVINCES: ReadonlyArray<readonly [string, string]> = [
  ['AB', 'Alberta'], ['BC', 'British Columbia'], ['MB', 'Manitoba'], ['NB', 'New Brunswick'],
  ['NL', 'Newfoundland and Labrador'], ['NS', 'Nova Scotia'], ['NT', 'Northwest Territories'],
  ['NU', 'Nunavut'], ['ON', 'Ontario'], ['PE', 'Prince Edward Island'], ['QC', 'Quebec'],
  ['SK', 'Saskatchewan'], ['YT', 'Yukon'],
]

export const IN_STATES: ReadonlyArray<readonly [string, string]> = [
  ['AP', 'Andhra Pradesh'], ['AR', 'Arunachal Pradesh'], ['AS', 'Assam'], ['BR', 'Bihar'],
  ['CT', 'Chhattisgarh'], ['GA', 'Goa'], ['GJ', 'Gujarat'], ['HR', 'Haryana'], ['HP', 'Himachal Pradesh'],
  ['JK', 'Jammu & Kashmir'], ['JH', 'Jharkhand'], ['KA', 'Karnataka'], ['KL', 'Kerala'], ['MP', 'Madhya Pradesh'],
  ['MH', 'Maharashtra'], ['MN', 'Manipur'], ['ML', 'Meghalaya'], ['MZ', 'Mizoram'], ['NL', 'Nagaland'],
  ['OD', 'Odisha'], ['PB', 'Punjab'], ['RJ', 'Rajasthan'], ['SK', 'Sikkim'], ['TN', 'Tamil Nadu'],
  ['TG', 'Telangana'], ['TR', 'Tripura'], ['UP', 'Uttar Pradesh'], ['UK', 'Uttarakhand'], ['WB', 'West Bengal'],
  ['DL', 'Delhi'], ['CH', 'Chandigarh'], ['PY', 'Puducherry'],
]

export function statesForCountry(country: string): ReadonlyArray<readonly [string, string]> | null {
  if (country === 'US') return US_STATES
  if (country === 'CA') return CA_PROVINCES
  if (country === 'IN') return IN_STATES
  return null
}

export function stateLabelForCountry(country: string): string {
  if (country === 'CA') return 'Province'
  return 'State'
}

export function postalPlaceholderForCountry(country: string): string {
  switch (country) {
    case 'US': return '94105'
    case 'CA': return 'M5V 3A8'
    case 'GB': return 'SW1A 1AA'
    case 'IN': return '400001'
    case 'DE': return '10115'
    case 'FR': return '75001'
    case 'AU': return '2000'
    case 'JP': return '100-0001'
    case 'BR': return '01310-100'
    case 'MX': return '06000'
    default: return 'Postal code'
  }
}

// Currencies sorted by billing volume. Label combines code + symbol + name
// for the Combobox search (user can type "euro", "eur", "€", "european").
export const CURRENCIES: ReadonlyArray<{
  code: CurrencyCode
  label: string
  symbol: string
}> = [
  { code: 'USD', label: 'US Dollar', symbol: '$' },
  { code: 'EUR', label: 'Euro', symbol: '\u20AC' },
  { code: 'GBP', label: 'British Pound', symbol: '\u00A3' },
  { code: 'CAD', label: 'Canadian Dollar', symbol: 'CA$' },
  { code: 'AUD', label: 'Australian Dollar', symbol: 'A$' },
  { code: 'INR', label: 'Indian Rupee', symbol: '\u20B9' },
  { code: 'CNY', label: 'Chinese Yuan', symbol: '\u00A5' },
  { code: 'CHF', label: 'Swiss Franc', symbol: 'CHF' },
  { code: 'SGD', label: 'Singapore Dollar', symbol: 'S$' },
  { code: 'HKD', label: 'Hong Kong Dollar', symbol: 'HK$' },
  { code: 'NZD', label: 'New Zealand Dollar', symbol: 'NZ$' },
  { code: 'BRL', label: 'Brazilian Real', symbol: 'R$' },
  { code: 'MXN', label: 'Mexican Peso', symbol: 'MX$' },
  { code: 'SEK', label: 'Swedish Krona', symbol: 'kr' },
  { code: 'NOK', label: 'Norwegian Krone', symbol: 'kr' },
  { code: 'DKK', label: 'Danish Krone', symbol: 'kr' },
  { code: 'PLN', label: 'Polish Zloty', symbol: 'z\u0142' },
  { code: 'CZK', label: 'Czech Koruna', symbol: 'K\u010d' },
  { code: 'ILS', label: 'Israeli Shekel', symbol: '\u20AA' },
  { code: 'AED', label: 'UAE Dirham', symbol: 'AED' },
  { code: 'SAR', label: 'Saudi Riyal', symbol: 'SAR' },
  { code: 'ZAR', label: 'South African Rand', symbol: 'R' },
  { code: 'THB', label: 'Thai Baht', symbol: '\u0E3F' },
  { code: 'IDR', label: 'Indonesian Rupiah', symbol: 'Rp' },
  { code: 'MYR', label: 'Malaysian Ringgit', symbol: 'RM' },
  { code: 'PHP', label: 'Philippine Peso', symbol: '\u20B1' },
]

export const CURRENCY_BY_CODE: Record<string, typeof CURRENCIES[number]> =
  Object.fromEntries(CURRENCIES.map(c => [c.code, c]))

// Smart-default tax labels by country. Used to pre-fill tax_name when the
// operator sets their home country. Purely a UX hint — they can override.
export const DEFAULT_TAX_NAME_BY_COUNTRY: Record<string, string> = {
  IN: 'GST', SG: 'GST', AU: 'GST', NZ: 'GST', CA: 'GST/HST',
  US: 'Sales Tax',
  GB: 'VAT', IE: 'VAT', DE: 'VAT', FR: 'VAT', ES: 'VAT', IT: 'VAT',
  NL: 'VAT', BE: 'VAT', SE: 'VAT', NO: 'VAT', DK: 'VAT', FI: 'VAT',
  PL: 'VAT', PT: 'VAT', CH: 'VAT', AT: 'VAT', CZ: 'VAT', GR: 'VAT',
  AE: 'VAT', SA: 'VAT', ZA: 'VAT',
  JP: 'Consumption Tax', KR: 'VAT',
  BR: 'ICMS', MX: 'IVA', AR: 'IVA', CL: 'IVA', CO: 'IVA',
}

// Typical VAT/GST rate percentages by country. Used as a hint in the UI
// when the operator sets their home country — not authoritative, since
// rates change and jurisdictions have multiple tiers.
export const TYPICAL_TAX_RATE_BY_COUNTRY: Record<string, number> = {
  GB: 20, IE: 23, DE: 19, FR: 20, ES: 21, IT: 22, NL: 21, BE: 21,
  SE: 25, NO: 25, DK: 25, FI: 24, PL: 23, PT: 23, CH: 8.1, AT: 20,
  CZ: 21, GR: 24,
  IN: 18, SG: 9, AU: 10, NZ: 15, JP: 10, KR: 10,
  AE: 5, SA: 15, ZA: 15,
}

// Timezone data backed by @vvo/tzdb: full IANA metadata (country, cities,
// abbreviations, DST-aware offsets, legacy aliases). Library is updated
// with each IANA release, eliminating hand-maintained rename/alias maps.
const TZ_DATA: TimeZone[] = getTimeZones({ includeUtc: true })

// Reverse index: any alias (including legacy names like Asia/Calcutta) →
// the modern canonical name (Asia/Kolkata). Built once on module load.
const TZ_CANONICAL: Record<string, string> = (() => {
  const out: Record<string, string> = {}
  for (const z of TZ_DATA) {
    for (const alias of z.group) out[alias] = z.name
    out[z.name] = z.name
  }
  return out
})()

const TZ_BY_NAME: Record<string, TimeZone> = Object.fromEntries(TZ_DATA.map(z => [z.name, z]))

// Canonical IANA zone names, alphabetically sorted. Deduped via Set
// because @vvo/tzdb's getTimeZones can return multiple entries that
// resolve to the same canonical name (the library lists primary
// zones AND legacy aliases that share a canonical) — without the
// dedupe the Timezone Combobox emitted duplicate React keys
// (e.g. "Africa/Nairobi") and warned about non-unique children.
export const TIMEZONES: string[] = Array.from(new Set(TZ_DATA.map(z => z.name))).sort()

// Remap any IANA name (including legacy aliases like Asia/Calcutta) to its
// modern canonical form. Pass-through if unrecognized. Use when hydrating
// stored values so old records display with their current canonical name.
export function normalizeTimezone(tz: string): string {
  return TZ_CANONICAL[tz] ?? tz
}

function formatOffset(minutes: number): string {
  if (minutes === 0) return 'GMT'
  const sign = minutes > 0 ? '+' : '-'
  const abs = Math.abs(minutes)
  const h = Math.floor(abs / 60)
  const m = abs % 60
  return m === 0 ? `GMT${sign}${h}` : `GMT${sign}${h}:${String(m).padStart(2, '0')}`
}

// Build a Combobox option for a timezone. Label shows canonical name +
// DST-aware offset; keywords include country, cities, abbreviation, and
// legacy aliases so search finds the zone by any common term (e.g.
// "India", "IST", "Calcutta", "Mumbai" all find Asia/Kolkata).
export function timezoneOption(tz: string): { value: string; label: string; keywords: string[] } {
  const canonical = normalizeTimezone(tz)
  const z = TZ_BY_NAME[canonical]
  if (!z) {
    const pretty = tz.replace(/_/g, ' ')
    return { value: tz, label: pretty, keywords: [tz, pretty] }
  }
  const offset = formatOffset(z.currentTimeOffsetInMinutes)
  const pretty = z.name.replace(/_/g, ' ')
  return {
    value: z.name,
    label: `${pretty} (${offset})`,
    keywords: [
      z.name,
      pretty,
      offset,
      z.countryName,
      z.countryCode,
      z.abbreviation,
      z.alternativeName,
      ...z.mainCities,
      ...z.group,
    ].filter(Boolean),
  }
}
