import { useEffect, useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import type { ReactNode } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, setActiveCurrency, setTenantTimezone, formatCents, formatTaxRate, type StripeProviderCredentials } from '@/lib/api'
import { applyApiError, showApiError } from '@/lib/formErrors'
import { Layout } from '@/components/Layout'
import { useAuth } from '@/contexts/AuthContext'
import { toast } from 'sonner'
import { cn } from '@/lib/utils'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { Card, CardContent } from '@/components/ui/card'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Switch } from '@/components/ui/switch'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Combobox } from '@/components/Combobox'
import {
  COUNTRIES,
  COUNTRY_NAME,
  CURRENCIES as GEO_CURRENCIES,
  CURRENCY_BY_CODE,
  TIMEZONES,
  timezoneOption,
  normalizeTimezone,
  DEFAULT_TAX_NAME_BY_COUNTRY,
  TYPICAL_TAX_RATE_BY_COUNTRY,
  statesForCountry,
  stateLabelForCountry,
  postalPlaceholderForCountry,
} from '@/lib/geo'

import { Building2, CreditCard, FileText, Receipt, Check, AlertCircle, Loader2, Trash2, Copy, ExternalLink, X, Users } from 'lucide-react'
import { TeamTab } from '@/components/TeamTab'
import { CONTENT_BAND } from '@/lib/contentBand'

const settingsSchema = z.object({
  company_name: z.string().max(255, 'Must be at most 255 characters'),
  company_email: z.string().email('Invalid email address').or(z.literal('')),
  company_phone: z.string().regex(/^[+\d\s()-]{7,20}$/, 'Invalid phone number').or(z.literal('')),
  company_address_line1: z.string().max(200, 'Must be at most 200 characters'),
  company_address_line2: z.string().max(200, 'Must be at most 200 characters'),
  company_city: z.string().max(100, 'Must be at most 100 characters'),
  company_state: z.string().max(100, 'Must be at most 100 characters'),
  company_postal_code: z.string().max(20, 'Must be at most 20 characters'),
  company_country: z.string().max(2, 'Must be an ISO-3166 alpha-2 code'),
  logo_url: z.string().max(500, 'Must be at most 500 characters'),
  brand_color: z.string().regex(/^(#[0-9a-f]{6})?$/, 'Must be a 7-character hex like #1f6feb').or(z.literal('')),
  tax_id: z.string().max(50, 'Must be at most 50 characters'),
  support_url: z.string().regex(/^https?:\/\/.+/i, 'Must start with http:// or https://').or(z.literal('')),
  invoice_footer: z.string().max(1000, 'Must be at most 1000 characters'),
  invoice_prefix: z.string()
    .max(20, 'Must be at most 20 characters')
    .regex(/^[A-Z0-9-]*$/, 'Letters, numbers, and hyphens only'),
  net_payment_terms: z.number().min(0).max(365),
  tax_provider: z.enum(['none', 'manual', 'stripe_tax']),
  tax_rate: z.number().min(0).max(100),
  tax_name: z.string().max(50, 'Must be at most 50 characters'),
  tax_inclusive: z.boolean(),
  default_product_tax_code: z.string()
    .regex(/^(txcd_\d{8})?$/, 'Must be a Stripe tax code like txcd_10103001, or empty'),
  default_currency: z.string(),
  timezone: z.string(),
  // Dollar amount (string-bound input); '' = low-balance alerts off.
  credit_balance_low_threshold: z.string()
    .regex(/^\d*(\.\d{1,2})?$/, 'Dollar amount like 50 or 50.00')
    .or(z.literal('')),
})

type SettingsFormData = z.infer<typeof settingsSchema>

// Prefer the deployment-configured public API origin (VITE_API_URL) over
// window.location.origin — in dev the browser loads from Vite at :5173 while
// the Go API runs on :8080, so window.location.origin produces a misleading
// value for both the "API Endpoint" display and the Stripe Webhook URL
// (Stripe can't reach a dev-server port). Falls back to the current origin
// so production deployments that don't set the env var still render.
const API_ORIGIN = import.meta.env.VITE_API_URL || window.location.origin

// Common net-term presets. Covers 95% of real-world billing setups; the
// custom field below handles everything else (e.g. Net 45, Net 90).
const PAYMENT_TERM_PRESETS = [
  { label: 'Due on receipt', value: 0 },
  { label: 'Net 7', value: 7 },
  { label: 'Net 15', value: 15 },
  { label: 'Net 30', value: 30 },
  { label: 'Net 60', value: 60 },
]

// Render a small thumbnail of the logo URL so operators can verify the
// link resolves before saving — catches typos, broken CDN paths, and
// auth-gated URLs that won't render on invoice PDFs.
// Mounted with key={url} by the caller: a URL change REMOUNTS the
// preview, so status naturally restarts at 'loading' — no reset effect
// (the old effect set state synchronously post-render, briefly showing
// the previous URL's loaded/error state against the new URL).
function LogoPreview({ url }: { url: string }) {
  const [status, setStatus] = useState<'loading' | 'loaded' | 'error'>('loading')
  if (!url || !/^https?:\/\//i.test(url)) return null
  return (
    <div className="mt-2 flex items-center gap-2">
      <div className="h-10 w-20 rounded-md border border-border bg-muted/30 flex items-center justify-center overflow-hidden">
        {status === 'error' ? (
          <AlertCircle size={14} className="text-destructive" aria-label="Failed to load" />
        ) : (
          <img
            src={url}
            alt="Logo preview"
            className={cn(
              'max-h-8 max-w-[72px] object-contain',
              status === 'loading' && 'opacity-0',
            )}
            onLoad={() => setStatus('loaded')}
            onError={() => setStatus('error')}
          />
        )}
      </div>
      <span className={cn(
        'text-xs',
        status === 'error' ? 'text-destructive' : 'text-muted-foreground',
      )}>
        {status === 'loaded' && 'Preview'}
        {status === 'error' && "Couldn't load image"}
        {status === 'loading' && 'Loading...'}
      </span>
    </div>
  )
}

export default function SettingsPage() {
  usePageTitle('Settings')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [searchParams, setSearchParams] = useSearchParams()

  const formMethods = useForm<SettingsFormData>({
    resolver: zodResolver(settingsSchema),
    defaultValues: {
      company_name: '', company_email: '', company_phone: '',
      company_address_line1: '', company_address_line2: '',
      company_city: '', company_state: '', company_postal_code: '', company_country: '',
      logo_url: '', brand_color: '', tax_id: '', support_url: '', invoice_footer: '',
      invoice_prefix: '', net_payment_terms: 0,
      tax_provider: 'manual', tax_rate: 0, tax_name: '', tax_inclusive: false,
      default_product_tax_code: '',
      default_currency: '', timezone: '',
      credit_balance_low_threshold: '',
    },
  })
  const { register, handleSubmit, watch, reset, setValue, formState: { errors: formErrors, isDirty } } = formMethods

  const form = watch()
  const hasChanges = isDirty

  useEffect(() => {
    const handleBeforeUnload = (e: BeforeUnloadEvent) => {
      if (hasChanges) { e.preventDefault(); e.returnValue = '' }
    }
    window.addEventListener('beforeunload', handleBeforeUnload)
    return () => window.removeEventListener('beforeunload', handleBeforeUnload)
  }, [hasChanges])

  const loadSettings = () => {
    setLoading(true); setError(null)
    api.getSettings().then(s => {
      const f = {
        company_name: s.company_name || '', company_email: s.company_email || '',
        company_phone: s.company_phone || '',
        company_address_line1: s.company_address_line1 || '',
        company_address_line2: s.company_address_line2 || '',
        company_city: s.company_city || '',
        company_state: s.company_state || '',
        company_postal_code: s.company_postal_code || '',
        company_country: s.company_country || '',
        logo_url: s.logo_url || '',
        brand_color: (s.brand_color || '').toLowerCase(),
        tax_id: s.tax_id || '', support_url: s.support_url || '', invoice_footer: s.invoice_footer || '',
        invoice_prefix: s.invoice_prefix || '', net_payment_terms: s.net_payment_terms || 0,
        tax_provider: (s.tax_provider as 'none' | 'manual' | 'stripe_tax') || 'manual',
        tax_rate: s.tax_rate || 0, tax_name: s.tax_name || '', tax_inclusive: s.tax_inclusive || false,
        default_product_tax_code: s.default_product_tax_code || '',
        default_currency: s.default_currency || '', timezone: s.timezone ? normalizeTimezone(s.timezone) : '',
        credit_balance_low_threshold: s.credit_balance_low_threshold_cents != null
          ? (s.credit_balance_low_threshold_cents / 100).toString()
          : '',
      }
      reset(f)
      setLoading(false)
    }).catch(err => { setError(err instanceof Error ? err.message : 'Failed to load settings'); setLoading(false) })
  }

  useEffect(() => { loadSettings() }, [])

  const handleSave = handleSubmit(async (data) => {
    setSaving(true)
    try {
      // Dollar string -> cents (null clears the threshold: alerts off).
      const { credit_balance_low_threshold, ...rest } = data
      const updated = await api.updateSettings({
        ...rest,
        credit_balance_low_threshold_cents: credit_balance_low_threshold === ''
          ? null
          : Math.round(parseFloat(credit_balance_low_threshold) * 100),
      })
      const f = {
        company_name: updated.company_name || '', company_email: updated.company_email || '',
        company_phone: updated.company_phone || '',
        company_address_line1: updated.company_address_line1 || '',
        company_address_line2: updated.company_address_line2 || '',
        company_city: updated.company_city || '',
        company_state: updated.company_state || '',
        company_postal_code: updated.company_postal_code || '',
        company_country: updated.company_country || '',
        logo_url: updated.logo_url || '',
        brand_color: (updated.brand_color || '').toLowerCase(),
        tax_id: updated.tax_id || '', support_url: updated.support_url || '', invoice_footer: updated.invoice_footer || '',
        invoice_prefix: updated.invoice_prefix || '', net_payment_terms: updated.net_payment_terms || 0,
        tax_provider: (updated.tax_provider as 'none' | 'manual' | 'stripe_tax') || 'manual',
        tax_rate: updated.tax_rate || 0, tax_name: updated.tax_name || '', tax_inclusive: updated.tax_inclusive || false,
        default_product_tax_code: updated.default_product_tax_code || '',
        default_currency: updated.default_currency || '', timezone: updated.timezone ? normalizeTimezone(updated.timezone) : '',
        credit_balance_low_threshold: updated.credit_balance_low_threshold_cents != null
          ? (updated.credit_balance_low_threshold_cents / 100).toString()
          : '',
      }
      reset(f)
      if (updated.default_currency) setActiveCurrency(updated.default_currency)
      // Synchronously reseed the module-scoped tenant TZ so the very
      // next render of any page (including a navigation that happens
      // before any async refetch could land) uses the new value.
      // TenantTimezoneBootstrap is the only consumer of the shared
      // ['settings'] query, so the module-state write is the single
      // authoritative path — no parallel cache invalidation needed.
      setTenantTimezone(updated.timezone ? normalizeTimezone(updated.timezone) : null)
      toast.success('Settings saved')
    } catch (err) {
      applyApiError(formMethods, err, [
        'company_name', 'company_email', 'company_phone',
        'company_address_line1', 'company_address_line2', 'company_city',
        'company_state', 'company_postal_code', 'company_country',
        'logo_url', 'brand_color',
        'tax_id', 'support_url', 'invoice_footer',
        'invoice_prefix', 'net_payment_terms', 'tax_provider', 'tax_rate', 'tax_name', 'tax_inclusive',
        'default_product_tax_code', 'default_currency', 'timezone',
      ], { toastTitle: 'Failed to save settings' })
    } finally { setSaving(false) }
  })

  if (loading) return (
    <Layout>
      <h1 className="text-2xl font-semibold text-foreground">Settings</h1>
      <div className="mt-6 flex justify-center p-8">
        <Loader2 className="h-6 w-6 animate-spin text-muted-foreground" />
      </div>
    </Layout>
  )

  if (error) return (
    <Layout>
      <h1 className="text-2xl font-semibold text-foreground">Settings</h1>
      <div className="mt-6 p-8 text-center">
        <p className="text-sm text-destructive mb-3">{error}</p>
        <Button variant="outline" size="sm" onClick={loadSettings}>Retry</Button>
      </div>
    </Layout>
  )

  const currencyObj = CURRENCY_BY_CODE[form.default_currency]
  const symbol = currencyObj?.symbol || '$'
  const statesList = statesForCountry(form.company_country)
  const stateLabel = stateLabelForCountry(form.company_country)
  const postalPlaceholder = postalPlaceholderForCountry(form.company_country)
  // Tax suggestions key off company_country — that's our single source of
  // truth for the seller's jurisdiction (removed redundant tax_home_country).
  const suggestedTaxName = form.company_country ? DEFAULT_TAX_NAME_BY_COUNTRY[form.company_country] : undefined
  const typicalRate = form.company_country ? TYPICAL_TAX_RATE_BY_COUNTRY[form.company_country] : undefined

  // Tab values: 'general' is the default home; 'business' is accepted
  // as a back-compat alias since older bookmarks / external links
  // (docs, support emails) point at ?tab=business.
  const tab = ((): 'general' | 'invoicing' | 'tax' | 'payments' | 'team' => {
    const q = searchParams.get('tab')
    if (q === 'invoicing' || q === 'tax' || q === 'payments' || q === 'team') return q
    if (q === 'business') return 'general'
    return 'general'
  })()
  const setTab = (t: string) => setSearchParams(t === 'general' ? {} : { tab: t })

  const countryOptions = COUNTRIES.map(([code, name]) => ({
    value: code,
    label: name,
    keywords: [code],
  }))
  const currencyOptions = GEO_CURRENCIES.map(c => ({
    value: c.code,
    label: `${c.label} (${c.code})`,
    keywords: [c.code, c.symbol, c.label],
    prefix: <span className="text-muted-foreground font-mono text-[11px] w-7 inline-block text-center">{c.symbol}</span>,
  }))
  const timezoneOptions = TIMEZONES.map(timezoneOption)
  const stateOptions = statesList
    ? statesList.map(([code, name]) => ({ value: code, label: name, keywords: [code] }))
    : null

  return (
    <Layout>
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Settings</h1>
          <p className="text-sm text-muted-foreground mt-1">Configure your billing tenant</p>
        </div>
        {!hasChanges && !loading && (
          <span className="text-sm text-emerald-600 dark:text-emerald-400 flex items-center gap-1.5 bg-emerald-50 dark:bg-emerald-500/10 px-3 py-1.5 rounded-lg">
            <Check size={14} /> Saved
          </span>
        )}
      </div>

      <Tabs value={tab} onValueChange={setTab} className="mt-6">
        <TabsList>
          <TabsTrigger value="general"><Building2 size={14} /> General</TabsTrigger>
          <TabsTrigger value="invoicing"><FileText size={14} /> Invoicing</TabsTrigger>
          <TabsTrigger value="tax"><Receipt size={14} /> Tax</TabsTrigger>
          <TabsTrigger value="payments"><CreditCard size={14} /> Payments</TabsTrigger>
          <TabsTrigger value="team"><Users size={14} /> Team</TabsTrigger>
        </TabsList>

        <div className="max-w-3xl mt-6 pb-24">

          {/* General: identity, address, and tenant-wide preferences
              (timezone). "Business" was misleading — timezone affects
              every formatDate in the dashboard, not just invoicing. */}
          <TabsContent value="general" className="space-y-6">
            <Card>
              <CardContent className="p-6">
                <div className="mb-5">
                  <h2 className="text-sm font-semibold text-foreground">Business info</h2>
                  <p className="text-xs text-muted-foreground mt-0.5">Shown on invoices and customer-facing emails</p>
                </div>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-5">
                  <div>
                    <Label>Business name</Label>
                    <Input type="text" placeholder="Acme Inc." maxLength={255}
                      {...register('company_name')}
                      className={cn('mt-1', formErrors.company_name && 'border-destructive')} />
                    {formErrors.company_name
                      ? <p className="text-xs text-destructive mt-1">{formErrors.company_name.message}</p>
                      : <p className="text-xs text-muted-foreground mt-1">Displayed on invoice headers</p>}
                  </div>
                  <div>
                    <Label>Email</Label>
                    <Input type="email" placeholder="billing@acme.com" maxLength={254}
                      {...register('company_email')}
                      className={cn('mt-1', formErrors.company_email && 'border-destructive')} />
                    {formErrors.company_email
                      ? <p className="text-xs text-destructive mt-1">{formErrors.company_email.message}</p>
                      : <p className="text-xs text-muted-foreground mt-1">Reply-to address on invoice emails</p>}
                  </div>
                  <div>
                    <Label>Phone</Label>
                    <Input type="tel" placeholder="+1 (555) 123-4567" maxLength={20}
                      {...register('company_phone')}
                      className={cn('mt-1', formErrors.company_phone && 'border-destructive')} />
                    {formErrors.company_phone && <p className="text-xs text-destructive mt-1">{formErrors.company_phone.message}</p>}
                  </div>
                  <div>
                    <Label>Support URL</Label>
                    <Input type="url" placeholder="https://acme.com/support" maxLength={500}
                      {...register('support_url')}
                      className={cn('mt-1', formErrors.support_url && 'border-destructive')} />
                    {formErrors.support_url
                      ? <p className="text-xs text-destructive mt-1">{formErrors.support_url.message}</p>
                      : <p className="text-xs text-muted-foreground mt-1">Shown in invoice footers so customers can find help</p>}
                  </div>
                  <div className="md:col-span-2">
                    <Label>Logo URL</Label>
                    <Input type="url" placeholder="https://acme.com/logo.png" maxLength={500}
                      {...register('logo_url')}
                      className={cn('mt-1', formErrors.logo_url && 'border-destructive')} />
                    {formErrors.logo_url
                      ? <p className="text-xs text-destructive mt-1">{formErrors.logo_url.message}</p>
                      : form.logo_url && /^https?:\/\//i.test(form.logo_url)
                        ? <LogoPreview key={form.logo_url} url={form.logo_url} />
                        : <p className="text-xs text-muted-foreground mt-1">Public HTTPS URL — e.g. Cloudinary, S3 public object, or your CDN. Rendered at the top of invoice PDFs.</p>}
                  </div>
                  <div className="md:col-span-2">
                    <Label>Brand color</Label>
                    <div className="mt-1 flex items-center gap-2">
                      <input
                        type="color"
                        aria-label="Brand color picker"
                        value={/^#[0-9a-f]{6}$/i.test(form.brand_color || '') ? (form.brand_color as string).toLowerCase() : '#1f6feb'}
                        onChange={e => setValue('brand_color', e.target.value.toLowerCase(), { shouldDirty: true })}
                        className="h-9 w-12 cursor-pointer rounded-md border border-border bg-background p-1"
                      />
                      <Input
                        type="text"
                        placeholder="#1f6feb"
                        maxLength={7}
                        value={form.brand_color || ''}
                        onChange={e => setValue('brand_color', e.target.value.toLowerCase(), { shouldDirty: true })}
                        className={cn('w-32 font-mono text-sm', formErrors.brand_color && 'border-destructive')}
                      />
                      {form.brand_color && (
                        <button
                          type="button"
                          onClick={() => setValue('brand_color', '', { shouldDirty: true })}
                          className="text-xs text-muted-foreground hover:text-foreground"
                        >
                          Clear
                        </button>
                      )}
                    </div>
                    {formErrors.brand_color
                      ? <p className="text-xs text-destructive mt-1">{formErrors.brand_color.message}</p>
                      : <p className="text-xs text-muted-foreground mt-1">Accent color on invoice PDFs. Leave blank for the neutral default.</p>}
                  </div>
                </div>
              </CardContent>
            </Card>

            <Card>
              <CardContent className="p-6">
                <div className="mb-5">
                  <h2 className="text-sm font-semibold text-foreground">Address</h2>
                  <p className="text-xs text-muted-foreground mt-0.5">Appears in the "From" block on invoice PDFs</p>
                </div>
                <div className="grid grid-cols-1 md:grid-cols-6 gap-5">
                  <div className="md:col-span-6">
                    <Label>Country</Label>
                    <Combobox
                      value={form.company_country}
                      onChange={v => setValue('company_country', v, { shouldDirty: true })}
                      options={countryOptions}
                      placeholder="Select country..."
                      clearable
                      className="mt-1"
                    />
                    {formErrors.company_country && <p className="text-xs text-destructive mt-1">{formErrors.company_country.message}</p>}
                  </div>
                  <div className="md:col-span-6">
                    <Label>Address line 1</Label>
                    <Input type="text" placeholder="123 Main Street" maxLength={200}
                      {...register('company_address_line1')}
                      className={cn('mt-1', formErrors.company_address_line1 && 'border-destructive')} />
                    {formErrors.company_address_line1 && <p className="text-xs text-destructive mt-1">{formErrors.company_address_line1.message}</p>}
                  </div>
                  <div className="md:col-span-6">
                    <Label>Address line 2 <span className="text-muted-foreground font-normal">(optional)</span></Label>
                    <Input type="text" placeholder="Suite, floor, etc." maxLength={200}
                      {...register('company_address_line2')}
                      className={cn('mt-1', formErrors.company_address_line2 && 'border-destructive')} />
                  </div>
                  <div className="md:col-span-2">
                    <Label>City</Label>
                    <Input type="text" placeholder="San Francisco" maxLength={100}
                      {...register('company_city')}
                      className={cn('mt-1', formErrors.company_city && 'border-destructive')} />
                  </div>
                  <div className="md:col-span-2">
                    <Label>{stateLabel}</Label>
                    {stateOptions ? (
                      <Combobox
                        value={form.company_state}
                        onChange={v => setValue('company_state', v, { shouldDirty: true })}
                        options={stateOptions}
                        placeholder={`Select ${stateLabel.toLowerCase()}...`}
                        clearable
                        className="mt-1"
                      />
                    ) : (
                      <Input type="text" maxLength={100}
                        {...register('company_state')}
                        className={cn('mt-1', formErrors.company_state && 'border-destructive')} />
                    )}
                  </div>
                  <div className="md:col-span-2">
                    <Label>Postal code</Label>
                    <Input type="text" placeholder={postalPlaceholder} maxLength={20}
                      {...register('company_postal_code')}
                      className={cn('mt-1', formErrors.company_postal_code && 'border-destructive')} />
                  </div>
                </div>
              </CardContent>
            </Card>

            <Card>
              <CardContent className="p-6">
                <div className="mb-5">
                  <h2 className="text-sm font-semibold text-foreground">Preferences</h2>
                  <p className="text-xs text-muted-foreground mt-0.5">Tenant-wide display and date conventions</p>
                </div>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-5">
                  <div>
                    <Label>Timezone</Label>
                    <Combobox
                      value={form.timezone}
                      onChange={v => setValue('timezone', v, { shouldDirty: true })}
                      options={timezoneOptions}
                      placeholder="Select timezone..."
                      className="mt-1"
                    />
                    <p className="text-xs text-muted-foreground mt-1">All dashboard timestamps show in this zone.</p>
                  </div>
                </div>
              </CardContent>
            </Card>
          </TabsContent>

          {/* Invoicing: numbering, currency, locale, footer */}
          <TabsContent value="invoicing" className="space-y-6">
            <Card>
              <CardContent className="p-6">
                <div className="mb-5">
                  <h2 className="text-sm font-semibold text-foreground">Invoicing</h2>
                  <p className="text-xs text-muted-foreground mt-0.5">Currency, numbering, payment terms, and footer</p>
                </div>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-5">
                  <div>
                    <Label>Default currency</Label>
                    <Combobox
                      value={form.default_currency}
                      onChange={v => setValue('default_currency', v, { shouldDirty: true })}
                      options={currencyOptions}
                      placeholder="Select currency..."
                      className="mt-1"
                    />
                    <p className="text-xs text-muted-foreground mt-1">Used across invoices, plans, and charges</p>
                  </div>
                  <div>
                    <Label>Invoice prefix</Label>
                    <Input type="text" maxLength={20}
                      {...register('invoice_prefix', {
                        onChange: (e) => { e.target.value = e.target.value.toUpperCase() },
                      })}
                      className={cn('mt-1 font-mono uppercase', formErrors.invoice_prefix && 'border-destructive')}
                      placeholder="INV" />
                    {formErrors.invoice_prefix
                      ? <p className="text-xs text-destructive mt-1">{formErrors.invoice_prefix.message}</p>
                      : form.invoice_prefix
                        ? <p className="text-xs text-muted-foreground mt-1 font-mono">Preview: {form.invoice_prefix}-000001</p>
                        : <p className="text-xs text-muted-foreground mt-1">Letters, numbers, and hyphens only</p>}
                  </div>
                  <div>
                    <Label>Payment terms</Label>
                    <div className="flex flex-wrap gap-1.5 mt-1.5">
                      {PAYMENT_TERM_PRESETS.map(p => {
                        const active = form.net_payment_terms === p.value
                        return (
                          <button
                            key={p.value}
                            type="button"
                            onClick={() => setValue('net_payment_terms', p.value, { shouldDirty: true })}
                            className={cn(
                              'px-2.5 py-1 rounded-md border text-xs font-medium transition-colors',
                              active
                                ? 'border-primary bg-primary/5 text-primary'
                                : 'border-border text-muted-foreground hover:text-foreground hover:border-muted-foreground/50',
                            )}
                          >
                            {p.label}
                          </button>
                        )
                      })}
                    </div>
                    <div className="relative mt-2 max-w-[160px]">
                      <Input type="number" min={0} max={365}
                        {...register('net_payment_terms', { valueAsNumber: true })}
                        className="pr-14" />
                      <span className="absolute right-3 top-1/2 -translate-y-1/2 text-sm text-muted-foreground pointer-events-none">days</span>
                    </div>
                    <p className="text-xs text-muted-foreground mt-1.5">Days after issue before payment is due</p>
                  </div>
                  <div>
                    <Label>Credit low-balance alert</Label>
                    <div className="relative mt-1 max-w-[200px]">
                      <span className="absolute left-3 top-1/2 -translate-y-1/2 text-sm text-muted-foreground pointer-events-none">{symbol}</span>
                      <Input inputMode="decimal"
                        {...register('credit_balance_low_threshold')}
                        className={cn('pl-7', formErrors.credit_balance_low_threshold && 'border-destructive')}
                        placeholder="Off" />
                    </div>
                    {formErrors.credit_balance_low_threshold
                      ? <p className="text-xs text-destructive mt-1">{formErrors.credit_balance_low_threshold.message}</p>
                      : <p className="text-xs text-muted-foreground mt-1.5">Sends a webhook when a customer's credit balance drops below this amount. Leave blank to turn off. Empty and depleted balances always send their own events.</p>}
                  </div>
                  <div className="md:col-span-2">
                    <Label>Invoice footer</Label>
                    <Textarea
                      {...register('invoice_footer')}
                      rows={3}
                      maxLength={1000}
                      placeholder={"Thank you for your business.\nQuestions? billing@acme.com"}
                      className={cn('mt-1', formErrors.invoice_footer && 'border-destructive')} />
                    <div className="flex items-center justify-between mt-1">
                      {formErrors.invoice_footer
                        ? <p className="text-xs text-destructive">{formErrors.invoice_footer.message}</p>
                        : <p className="text-xs text-muted-foreground">Default footer on every invoice. Per-invoice overrides take precedence.</p>}
                      <p className="text-xs text-muted-foreground tabular-nums">{(form.invoice_footer || '').length}/1000</p>
                    </div>
                  </div>
                </div>
              </CardContent>
            </Card>
          </TabsContent>

          {/* Tax: tax ID, provider selection, rate (manual) or default tax code (stripe_tax) */}
          <TabsContent value="tax" className="space-y-6">
            <Card>
              <CardContent className="p-6">
                <div className="mb-5">
                  <h2 className="text-sm font-semibold text-foreground">Your tax identity</h2>
                  <p className="text-xs text-muted-foreground mt-0.5">
                    The tax ID you print on invoices. The seller's jurisdiction is your business address (set in General tab).
                  </p>
                </div>
                <div className="grid grid-cols-1 md:grid-cols-2 gap-5">
                  <div>
                    <Label>Tax ID</Label>
                    <Input type="text" placeholder="GB123456789 · 12-3456789 · 29ABCDE1234F1Z5" maxLength={50}
                      {...register('tax_id')}
                      className={cn('mt-1 font-mono', formErrors.tax_id && 'border-destructive')} />
                    {formErrors.tax_id
                      ? <p className="text-xs text-destructive mt-1">{formErrors.tax_id.message}</p>
                      : <p className="text-xs text-muted-foreground mt-1">Your VAT, EIN, GSTIN, or ABN — required on B2B invoices in most jurisdictions</p>}
                  </div>
                  <div>
                    <Label>Seller country</Label>
                    <div className="mt-1 px-3 py-2 rounded-md border border-border bg-muted/30 text-sm text-muted-foreground">
                      {form.company_country ? (COUNTRY_NAME[form.company_country] || form.company_country) : 'Set business address in General tab'}
                    </div>
                    <p className="text-xs text-muted-foreground mt-1">Drives tax name/rate suggestions and cross-border VAT handling</p>
                  </div>
                </div>
              </CardContent>
            </Card>

            <Card>
              <CardContent className="p-6">
                <div className="mb-5">
                  <h2 className="text-sm font-semibold text-foreground">Tax calculation</h2>
                  <p className="text-xs text-muted-foreground mt-0.5">How Velox adds tax to invoices</p>
                </div>
                <div>
                  <Label>Tax provider</Label>
                  <div className="mt-2 grid grid-cols-1 md:grid-cols-3 gap-3">
                    <button
                      type="button"
                      onClick={() => setValue('tax_provider', 'none', { shouldDirty: true })}
                      className={cn(
                        'text-left p-4 rounded-lg border transition-colors',
                        form.tax_provider === 'none'
                          ? 'border-primary bg-primary/5 ring-1 ring-primary'
                          : 'border-border hover:border-muted-foreground/50'
                      )}
                    >
                      <div className="font-medium text-sm text-foreground">None</div>
                      <div className="text-xs text-muted-foreground mt-1">Don't collect tax. Invoices ship with zero tax lines.</div>
                    </button>
                    <button
                      type="button"
                      onClick={() => setValue('tax_provider', 'manual', { shouldDirty: true })}
                      className={cn(
                        'text-left p-4 rounded-lg border transition-colors',
                        form.tax_provider === 'manual'
                          ? 'border-primary bg-primary/5 ring-1 ring-primary'
                          : 'border-border hover:border-muted-foreground/50'
                      )}
                    >
                      <div className="font-medium text-sm text-foreground">Manual (flat rate)</div>
                      <div className="text-xs text-muted-foreground mt-1">Single rate applied to all customers. Simplest option.</div>
                    </button>
                    <button
                      type="button"
                      onClick={() => setValue('tax_provider', 'stripe_tax', { shouldDirty: true })}
                      className={cn(
                        'text-left p-4 rounded-lg border transition-colors',
                        form.tax_provider === 'stripe_tax'
                          ? 'border-primary bg-primary/5 ring-1 ring-primary'
                          : 'border-border hover:border-muted-foreground/50'
                      )}
                    >
                      <div className="font-medium text-sm text-foreground flex items-center gap-2">
                        Stripe Tax
                        <span className="text-[10px] px-1.5 py-0.5 rounded bg-primary/10 text-primary font-semibold">AUTO</span>
                      </div>
                      <div className="text-xs text-muted-foreground mt-1">Jurisdiction-aware rates, reverse-charge, tax transactions. Requires Stripe in Payments tab.</div>
                    </button>
                  </div>
                </div>

                {form.tax_provider === 'manual' && (
                  <>
                    <div className="mt-6 grid grid-cols-1 md:grid-cols-2 gap-5">
                      <div>
                        <Label>Tax name</Label>
                        <Input type="text" maxLength={50} placeholder="e.g. GST, VAT, Sales Tax"
                          {...register('tax_name')}
                          className={cn('mt-1', formErrors.tax_name && 'border-destructive')} />
                        {formErrors.tax_name ? (
                          <p className="text-xs text-destructive mt-1">{formErrors.tax_name.message}</p>
                        ) : suggestedTaxName && form.tax_name !== suggestedTaxName ? (
                          <p className="text-xs text-muted-foreground mt-1">
                            Common in {COUNTRY_NAME[form.company_country] || form.company_country}:{' '}
                            <button
                              type="button"
                              className="underline underline-offset-2 hover:text-foreground"
                              onClick={() => setValue('tax_name', suggestedTaxName, { shouldDirty: true })}
                            >
                              use "{suggestedTaxName}"
                            </button>
                          </p>
                        ) : (
                          <p className="text-xs text-muted-foreground mt-1">Label shown on invoice line items</p>
                        )}
                      </div>
                      <div>
                        <Label>Tax rate</Label>
                        <div className="relative mt-1">
                          <Input type="number" step="0.0001" min={0} max={100}
                            value={form.tax_rate}
                            onChange={e => setValue('tax_rate', parseFloat(e.target.value || '0'), { shouldDirty: true })}
                            className="pr-8" placeholder="0" />
                          <span className="absolute right-3 top-1/2 -translate-y-1/2 text-sm text-muted-foreground pointer-events-none">%</span>
                        </div>
                        {typicalRate !== undefined && Math.abs(form.tax_rate - typicalRate) > 0.01 ? (
                          <p className="text-xs text-muted-foreground mt-1">
                            Typical in {COUNTRY_NAME[form.company_country] || form.company_country}:{' '}
                            <button
                              type="button"
                              className="underline underline-offset-2 hover:text-foreground"
                              onClick={() => setValue('tax_rate', typicalRate, { shouldDirty: true })}
                            >
                              use {typicalRate}%
                            </button>
                          </p>
                        ) : (
                          <p className="text-xs text-muted-foreground mt-1">Set to 0 to disable tax. Accepts up to 4 decimal places (e.g. 8.8750).</p>
                        )}
                      </div>
                    </div>
                    <div className="mt-5 flex items-start justify-between gap-4 p-4 rounded-lg border border-border bg-muted/30">
                      <div>
                        <Label className="font-medium text-foreground">Tax-inclusive pricing</Label>
                        <p className="text-xs text-muted-foreground mt-1">
                          When on, plan and usage prices already include tax. Velox carves tax out of the sticker price.
                          When off (default), tax is added on top of the subtotal.
                        </p>
                      </div>
                      <Switch
                        checked={form.tax_inclusive ?? false}
                        onCheckedChange={(checked) => setValue('tax_inclusive', !!checked, { shouldDirty: true })}
                        className="shrink-0 mt-0.5"
                      />
                    </div>
                    {form.tax_rate > 0 && (
                      <div className="mt-4 p-3 bg-amber-50 dark:bg-amber-500/10 border border-amber-100 dark:border-amber-500/20 rounded-lg">
                        <p className="text-xs text-amber-800 dark:text-amber-300 flex items-start gap-2">
                          <AlertCircle size={14} className="shrink-0 mt-0.5" />
                          {form.tax_inclusive ? (
                            <span>
                              Example: {symbol}100.00 sticker price includes {form.tax_name || 'tax'} at {formatTaxRate(form.tax_rate)}%.
                              Net subtotal = <strong>{formatCents(Math.round(10000 * 10000 / (10000 + form.tax_rate * 100)), form.default_currency || 'USD')}</strong>.
                              Customer pays <strong>{formatCents(10000, form.default_currency || 'USD')}</strong>.
                            </span>
                          ) : (
                            <span>
                              Example: {symbol}100.00 subtotal {form.tax_name ? `+ ${form.tax_name} ` : '+ tax '}
                              {formatTaxRate(form.tax_rate)}% = <strong>{formatCents(10000 + Math.round(10000 * (form.tax_rate * 100) / 10000), form.default_currency || 'USD')}</strong> total.
                            </span>
                          )}
                        </p>
                      </div>
                    )}
                  </>
                )}

                {form.tax_provider === 'stripe_tax' && (
                  <>
                    <div className="mt-6">
                      <Label>Default product tax code</Label>
                      <Input type="text" placeholder="txcd_10103001" maxLength={13}
                        {...register('default_product_tax_code')}
                        className={cn('mt-1 font-mono', formErrors.default_product_tax_code && 'border-destructive')} />
                      {formErrors.default_product_tax_code ? (
                        <p className="text-xs text-destructive mt-1">{formErrors.default_product_tax_code.message}</p>
                      ) : (
                        <p className="text-xs text-muted-foreground mt-1">
                          Fallback Stripe tax code used when a plan doesn't specify one.{' '}
                          <code className="font-mono">txcd_10103001</code> = SaaS business use.{' '}
                          <a href="https://docs.stripe.com/tax/tax-codes" target="_blank" rel="noreferrer"
                             className="underline underline-offset-2 hover:text-foreground inline-flex items-center gap-0.5">
                            Stripe tax code reference<ExternalLink size={10} />
                          </a>
                        </p>
                      )}
                    </div>
                    <div className="mt-5 flex items-start justify-between gap-4 p-4 rounded-lg border border-border bg-muted/30">
                      <div>
                        <Label className="font-medium text-foreground">Tax-inclusive pricing</Label>
                        <p className="text-xs text-muted-foreground mt-1">
                          When on, plan prices already include tax. Stripe Tax carves tax out of the gross amount.
                        </p>
                      </div>
                      <Switch
                        checked={form.tax_inclusive ?? false}
                        onCheckedChange={(checked) => setValue('tax_inclusive', !!checked, { shouldDirty: true })}
                        className="shrink-0 mt-0.5"
                      />
                    </div>
                    <div className="mt-4 p-3 bg-blue-50 dark:bg-blue-500/10 border border-blue-100 dark:border-blue-500/20 rounded-lg">
                      <p className="text-xs text-blue-800 dark:text-blue-300 flex items-start gap-2">
                        <AlertCircle size={14} className="shrink-0 mt-0.5" />
                        <span>
                          Tax is calculated per invoice via Stripe. If a calculation fails, the invoice stays in draft until it
                          resolves. Requires Stripe Tax enabled in your Stripe dashboard; registrations control which
                          jurisdictions collect tax.
                        </span>
                      </p>
                    </div>
                  </>
                )}
              </CardContent>
            </Card>
          </TabsContent>

          {/* Payments: per-tenant Stripe credentials */}
          <TabsContent value="payments">
            <PaymentsSection />
          </TabsContent>

          {/* Team: members + invitations. Lives outside the settings
              form — membership changes apply immediately, there's
              nothing to save. */}
          <TabsContent value="team">
            <TeamTab />
          </TabsContent>

        </div>
      </Tabs>

      {/* Sticky save bar. pr-44 reserves the right edge so the Save button
          doesn't sit under the OnboardingLauncher pill (also fixed
          bottom-right, z-30), keeping both accessible side-by-side. The
          geometry is self-evident; a previous version of this comment also
          claimed "Stripe/Linear use the same right-edge-reserve pattern",
          which an anchoring pass could find no citable documentation for —
          removed rather than left as an unsourced parity assertion. */}
      {hasChanges && (
        <div className="fixed bottom-0 left-0 right-0 z-30 md:left-60 pr-44">
          <div className={cn(CONTENT_BAND, 'px-4 md:px-8')}>
            <div className="bg-card border-t border-border shadow-[0_-4px_12px_rgba(0,0,0,0.05)] rounded-t-xl px-6 py-3 flex items-center justify-between">
              <div className="flex items-center gap-2">
                <span className="w-2 h-2 rounded-full bg-amber-500 animate-pulse" />
                <span className="text-sm text-muted-foreground">Unsaved changes</span>
              </div>
              <div className="flex items-center gap-3">
                <AlertDialog>
                  <AlertDialogTrigger render={<Button variant="ghost" />}>Discard</AlertDialogTrigger>
                  <AlertDialogContent>
                    <AlertDialogHeader>
                      <AlertDialogTitle>Discard changes?</AlertDialogTitle>
                      <AlertDialogDescription>
                        All unsaved edits on this page will be lost and the form reset to the last saved values.
                      </AlertDialogDescription>
                    </AlertDialogHeader>
                    <AlertDialogFooter>
                      <AlertDialogCancel>Keep editing</AlertDialogCancel>
                      <AlertDialogAction onClick={() => loadSettings()}>Discard changes</AlertDialogAction>
                    </AlertDialogFooter>
                  </AlertDialogContent>
                </AlertDialog>
                <Button onClick={() => handleSave()} disabled={saving}>
                  {saving ? <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</> : 'Save Changes'}
                </Button>
              </div>
            </div>
          </div>
        </div>
      )}
    </Layout>
  )
}

// ─────────────────────────────────────────────────────────────────────────
// Payments — per-tenant Stripe credentials
// ─────────────────────────────────────────────────────────────────────────
//
// One card for the mode the user is currently operating in (driven by the
// global Test/Live header toggle), not two stacked cards — switching modes
// is already a one-click header action. First-time setup is a two-step
// checklist (API keys → webhook); once wired, the card collapses to a
// summary row with a Manage button for rotates / disconnect.

function PaymentsSection() {
  const queryClient = useQueryClient()
  const { user } = useAuth()
  const livemode = user?.livemode ?? false
  const { data, isLoading, error } = useQuery({
    queryKey: ['stripe-credentials'],
    queryFn: () => api.listStripeCredentials(),
  })

  const current = data?.data?.find(r => r.livemode === livemode)
  const invalidate = () => queryClient.invalidateQueries({ queryKey: ['stripe-credentials'] })

  return (
    <section>
      <div className="flex items-center gap-2 mb-4">
        <div className="w-8 h-8 rounded-lg bg-violet-50 dark:bg-violet-500/10 flex items-center justify-center">
          <CreditCard size={16} className="text-violet-600 dark:text-violet-400" />
        </div>
        <div className="flex-1 min-w-0">
          <h2 className="text-sm font-semibold text-foreground">Payments</h2>
          <p className="text-xs text-muted-foreground">
            Connect your own Stripe account — Velox charges invoices through your keys, money flows straight to you.
            Showing <span className="font-medium text-foreground">{livemode ? 'live' : 'test'}</span> credentials;
            switch via the Test/Live toggle in the header.
          </p>
        </div>
      </div>

      {isLoading ? (
        <Card><CardContent className="p-6 flex justify-center"><Loader2 className="h-5 w-5 animate-spin text-muted-foreground" /></CardContent></Card>
      ) : error ? (
        <Card><CardContent className="p-6"><p className="text-sm text-destructive">{error instanceof Error ? error.message : 'Failed to load Stripe credentials'}</p></CardContent></Card>
      ) : (
        <StripeCard
          key={livemode ? 'live' : 'test'}
          livemode={livemode}
          current={current}
          onChanged={invalidate}
        />
      )}
    </section>
  )
}

// Prefix shape validated client-side before the request so operators see
// field-level errors without waiting for the server. Mirrors the backend's
// validateKeyShape — both must stay in sync.
// API keys and webhook secret are rotated independently — matches Stripe /
// Chargebee / Recurly UI. Webhook has its own form + PATCH endpoint.
const stripeConnectSchema = z.object({
  secret_key: z.string().min(1, 'Secret key is required'),
  publishable_key: z.string().min(1, 'Publishable key is required'),
})
type StripeConnectForm = z.infer<typeof stripeConnectSchema>

const webhookSecretSchema = z.object({
  webhook_secret: z.string().min(1, 'Webhook signing secret is required').startsWith('whsec_', 'Must start with whsec_'),
})
type WebhookSecretForm = z.infer<typeof webhookSecretSchema>

// Mirrors the dispatcher in internal/payment/stripe.go and the timeline filter
// in internal/invoice/handler.go. Keep in sync when backend adds new handlers —
// events not in this list are dropped at the default case (harmless but wasted
// round-trips from Stripe, and they won't surface on invoice timelines).
const STRIPE_WEBHOOK_EVENTS: { name: string; purpose: string }[] = [
  { name: 'payment_intent.succeeded', purpose: 'marks invoice paid' },
  { name: 'payment_intent.payment_failed', purpose: 'triggers dunning retry' },
  { name: 'payment_intent.canceled', purpose: 'shows on invoice timeline' },
  { name: 'checkout.session.completed', purpose: 'completes Stripe Checkout returns' },
  { name: 'setup_intent.succeeded', purpose: 'saves customer payment method' },
  { name: 'setup_intent.setup_failed', purpose: 'logs setup errors' },
  // Refund truth (ADR-063): a credit-note refund is created synchronously
  // but can FAIL later at Stripe (insufficient balance, dispute). Without
  // these, Velox keeps showing the refund as pending/succeeded forever —
  // and it only breaks in prod (stripe listen forwards ALL events, so dev
  // never reproduces it).
  { name: 'charge.refund.updated', purpose: 'tracks async refund status' },
  { name: 'refund.updated', purpose: 'tracks async refund status' },
  { name: 'refund.failed', purpose: 'flags refunds that failed at Stripe' },
]

// StripeCard is the main orchestrator for a single mode. State machine,
// from most → least common runtime path:
//
//   fullyConnected && !managing  → <ConnectedSummary/>  (collapsed row)
//   fullyConnected &&  managing  → full card with step summaries + rotate/disconnect actions
//   verified && webhookPending   → full card, step 1 summary, step 2 form
//   failing                       → full card with error banner, step 1 form
//   disconnected                  → full card, step 1 form, step 2 locked
//
// The `replacingKeys` / `replacingWebhook` booleans are inline editors
// inside steps 1 / 2 when the operator chooses to rotate a single credential
// from Manage mode — they swap the summary row for the matching form.
function StripeCard({ livemode, current, onChanged }: {
  livemode: boolean
  current: StripeProviderCredentials | undefined
  onChanged: () => void
}) {
  const [managing, setManaging] = useState(false)
  const [replacingKeys, setReplacingKeys] = useState(false)
  const [replacingWebhook, setReplacingWebhook] = useState(false)

  const connected = !!current
  const failing = connected && !!current.last_verified_error
  const verified = connected && !!current.verified_at && !current.last_verified_error
  const fullyConnected = verified && !!current.has_webhook_secret
  const webhookPending = verified && !current.has_webhook_secret

  if (fullyConnected && !managing && current) {
    return (
      <ConnectedSummary
        current={current}
        livemode={livemode}
        onManage={() => setManaging(true)}
      />
    )
  }

  const closeManage = () => {
    setManaging(false)
    setReplacingKeys(false)
    setReplacingWebhook(false)
  }

  // Step 1 shows the form when not yet verified OR the operator is rotating.
  const step1Mode: 'form' | 'summary' = !verified || replacingKeys ? 'form' : 'summary'

  // Step 2 is locked behind step 1; otherwise shows setup (URL + paste form)
  // when wiring is incomplete or the operator is rotating the webhook secret.
  const step2Mode: 'locked' | 'setup' | 'summary' =
    !verified ? 'locked'
    : (!current?.has_webhook_secret || replacingWebhook) ? 'setup'
    : 'summary'

  return (
    <Card>
      <CardContent className="p-6 space-y-5">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex items-center gap-2 flex-wrap">
              <h3 className="text-sm font-semibold text-foreground">
                Stripe — {livemode ? 'live' : 'test'} mode
              </h3>
              <StatusBadge
                connected={connected}
                failing={failing}
                fullyConnected={fullyConnected}
                webhookPending={webhookPending}
              />
            </div>
            <p className="text-xs text-muted-foreground mt-0.5">
              {livemode
                ? 'Production Stripe keys — real charges.'
                : 'Safe to experiment — no real money moves.'}
            </p>
          </div>
          {managing && (
            <button
              type="button"
              onClick={closeManage}
              aria-label="Close manage view"
              className="h-7 w-7 shrink-0 rounded-md text-muted-foreground hover:text-foreground hover:bg-accent flex items-center justify-center transition-colors"
            >
              <X size={16} />
            </button>
          )}
        </div>

        {failing && current && (
          <div className="p-3 rounded-lg bg-destructive/5 border border-destructive/20">
            <p className="text-xs text-destructive flex items-start gap-2">
              <AlertCircle size={12} className="shrink-0 mt-0.5" />
              <span>
                <strong>Stripe verification failed: </strong>
                {current.last_verified_error}. Re-paste your API keys below with
                the current values from your Stripe Dashboard.
              </span>
            </p>
          </div>
        )}

        <StepRow
          number={1}
          title="Connect your Stripe API keys"
          done={verified}
          summary={verified && current ? (
            <>
              Verified as{' '}
              <span className="font-medium text-foreground">
                {current.stripe_account_name || current.stripe_account_id}
              </span>
            </>
          ) : undefined}
        >
          {step1Mode === 'form' ? (
            <>
              {verified && managing && replacingKeys && (
                <div className="flex items-center justify-between text-xs text-muted-foreground mb-3">
                  <span>Paste the new keys you just rolled in Stripe.</span>
                  <button
                    type="button"
                    className="underline underline-offset-2 hover:text-foreground"
                    onClick={() => setReplacingKeys(false)}
                  >
                    Cancel
                  </button>
                </div>
              )}
              <ApiKeysForm
                livemode={livemode}
                connected={connected}
                onSuccess={() => { setReplacingKeys(false); onChanged() }}
              />
            </>
          ) : (
            managing && current && (
              <div className="space-y-3">
                <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                  <ReadOnlyRow label="Stripe account" value={current.stripe_account_name || current.stripe_account_id || '—'} />
                  <ReadOnlyRow
                    label="Secret key"
                    value={formatSecretKey(current.secret_key_prefix, current.secret_key_last4)}
                    mono={!!current.secret_key_last4}
                  />
                  <ReadOnlyRow label="Publishable key" value={current.publishable_key || '—'} mono />
                </div>
                <Button variant="outline" size="sm" onClick={() => setReplacingKeys(true)}>
                  Rotate API keys
                </Button>
              </div>
            )
          )}
        </StepRow>

        <StepRow
          number={2}
          title="Deliver Stripe events to Velox"
          done={fullyConnected}
          locked={step2Mode === 'locked'}
          summary={
            fullyConnected && current
              ? <>Signing secret <span className="font-mono text-foreground">whsec_••••{current.webhook_secret_last4 ?? ''}</span> active</>
              : step2Mode === 'locked'
                ? 'Unlocks after step 1'
                : undefined
          }
        >
          {step2Mode === 'setup' && current && (
            <>
              {managing && fullyConnected && replacingWebhook && (
                <div className="flex items-center justify-between text-xs text-muted-foreground mb-3">
                  <span>Paste the new signing secret from Stripe.</span>
                  <button
                    type="button"
                    className="underline underline-offset-2 hover:text-foreground"
                    onClick={() => setReplacingWebhook(false)}
                  >
                    Cancel
                  </button>
                </div>
              )}
              <WebhookSetup
                current={current}
                onSuccess={() => { setReplacingWebhook(false); onChanged() }}
              />
            </>
          )}
          {step2Mode === 'summary' && managing && current && (
            <div className="space-y-3">
              <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                <ReadOnlyRow
                  label="Signing secret"
                  value={`whsec_••••••••${current.webhook_secret_last4 ?? ''}`}
                  mono
                />
                <ReadOnlyRow label="Status" value="Verified" />
              </div>
              <WebhookUrlRow endpointId={current.id} />
              <Button variant="outline" size="sm" onClick={() => setReplacingWebhook(true)}>
                Replace webhook secret
              </Button>
            </div>
          )}
        </StepRow>

        {managing && connected && (
          <div className="pt-4 border-t border-border">
            <DisconnectAction mode={livemode ? 'live' : 'test'} onDone={onChanged} />
          </div>
        )}
      </CardContent>
    </Card>
  )
}

// Collapsed "happy path" — one-line summary plus Manage button. Pops open
// the full StripeCard (with step details + rotate/disconnect) on demand.
function ConnectedSummary({ current, livemode, onManage }: {
  current: StripeProviderCredentials
  livemode: boolean
  onManage: () => void
}) {
  return (
    <Card>
      <CardContent className="p-5 flex items-center justify-between gap-4">
        <div className="flex items-center gap-3 min-w-0">
          <div className="h-9 w-9 shrink-0 rounded-full bg-emerald-50 dark:bg-emerald-500/10 flex items-center justify-center">
            <Check size={16} className="text-emerald-600 dark:text-emerald-400" />
          </div>
          <div className="min-w-0">
            <div className="flex items-center gap-2 flex-wrap">
              <p className="text-sm font-semibold text-foreground truncate">
                Stripe {livemode ? 'live' : 'test'} mode connected
              </p>
              <span className="text-[11px] uppercase tracking-wide px-2 py-0.5 rounded-full bg-emerald-50 text-emerald-700 dark:bg-emerald-500/10 dark:text-emerald-400">
                Ready
              </span>
            </div>
            <p className="text-xs text-muted-foreground truncate">
              {current.stripe_account_name || current.stripe_account_id} ·{' '}
              <span className="font-mono">whsec_••••{current.webhook_secret_last4 ?? ''}</span>
            </p>
          </div>
        </div>
        <Button variant="outline" size="sm" onClick={onManage} className="shrink-0">
          Manage
        </Button>
      </CardContent>
    </Card>
  )
}

function StatusBadge({ connected, failing, fullyConnected, webhookPending }: {
  connected: boolean
  failing: boolean
  fullyConnected: boolean
  webhookPending: boolean
}) {
  if (failing) {
    return (
      <span className="text-[11px] uppercase tracking-wide px-2 py-0.5 rounded-full bg-destructive/10 text-destructive flex items-center gap-1">
        <AlertCircle size={10} /> Needs attention
      </span>
    )
  }
  if (fullyConnected) {
    return (
      <span className="text-[11px] uppercase tracking-wide px-2 py-0.5 rounded-full bg-emerald-50 text-emerald-700 dark:bg-emerald-500/10 dark:text-emerald-400 flex items-center gap-1">
        <Check size={10} /> Connected
      </span>
    )
  }
  if (webhookPending) {
    return (
      <span className="text-[11px] uppercase tracking-wide px-2 py-0.5 rounded-full bg-amber-50 text-amber-700 dark:bg-amber-500/10 dark:text-amber-400 flex items-center gap-1">
        <AlertCircle size={10} /> Webhook pending
      </span>
    )
  }
  if (connected) {
    return (
      <span className="text-[11px] uppercase tracking-wide px-2 py-0.5 rounded-full bg-muted text-muted-foreground">
        In progress
      </span>
    )
  }
  return (
    <span className="text-[11px] uppercase tracking-wide px-2 py-0.5 rounded-full bg-muted text-muted-foreground">
      Not connected
    </span>
  )
}

// Numbered step row. Locked steps render muted and without children; done
// steps render with a checkmark + one-line summary; active steps render
// their children underneath the title.
function StepRow({ number, title, done, locked, summary, children }: {
  number: number
  title: string
  done?: boolean
  locked?: boolean
  summary?: ReactNode
  children?: ReactNode
}) {
  return (
    <div className="flex items-start gap-3">
      <div
        className={cn(
          'h-6 w-6 shrink-0 rounded-full flex items-center justify-center text-xs font-medium mt-0.5',
          done
            ? 'bg-emerald-100 text-emerald-700 dark:bg-emerald-500/20 dark:text-emerald-400'
            : locked
              ? 'bg-muted text-muted-foreground'
              : 'bg-primary/10 text-primary ring-1 ring-primary/30',
        )}
      >
        {done ? <Check size={12} /> : number}
      </div>
      <div className="flex-1 min-w-0">
        <p className={cn('text-sm font-medium', locked ? 'text-muted-foreground' : 'text-foreground')}>
          {title}
        </p>
        {summary && (
          <p className="text-xs text-muted-foreground mt-0.5">{summary}</p>
        )}
        {!locked && children && (
          <div className="mt-3">{children}</div>
        )}
      </div>
    </div>
  )
}

// Secret-key + publishable-key form. Used both for first-time connect and
// for key rotation (mode switches based on `connected`). Rotations go through
// a confirmation dialog because pasting new keys invalidates the old ones
// the instant Stripe accepts them.
function ApiKeysForm({ livemode, connected, onSuccess }: {
  livemode: boolean
  connected: boolean
  onSuccess: () => void
}) {
  const methods = useForm<StripeConnectForm>({
    resolver: zodResolver(stripeConnectSchema),
    defaultValues: { secret_key: '', publishable_key: '' },
  })
  const { register, handleSubmit, reset, formState: { errors } } = methods
  const [pending, setPending] = useState<StripeConnectForm | null>(null)

  const connect = useMutation({
    mutationFn: (data: StripeConnectForm) => api.connectStripe({ ...data, livemode }),
    onSuccess: (row) => {
      if (row.last_verified_error) {
        toast.error('Saved, but Stripe could not verify these keys — see the details below the form.')
      } else {
        const base = `${connected ? 'Rotated' : 'Connected'} ${livemode ? 'live' : 'test'} mode${
          row.stripe_account_name ? ` as ${row.stripe_account_name}` : ''
        }`
        // ADR-019: when fresh credentials land, the server fans out
        // a background retry of any invoices that were stuck on
        // provider_not_configured / provider_auth. row.retries_queued
        // is the count it found at response time. Surface so the
        // operator knows the system is catching up — they don't
        // need to click Retry tax per invoice.
        const queued = row.retries_queued ?? 0
        const description = queued > 0
          ? queued === 1
            ? 'Retrying 1 invoice that was waiting for a Stripe connection.'
            : `Retrying ${queued} invoices that were waiting for a Stripe connection.`
          : undefined
        if (description) {
          toast.success(base, { description })
        } else {
          toast.success(base)
        }
      }
      reset()
      onSuccess()
    },
    onError: (err) => {
      applyApiError(methods, err, ['secret_key', 'publishable_key'], {
        toastTitle: 'Failed to connect Stripe',
      })
    },
  })

  const onSubmit = handleSubmit((data) => {
    if (connected) { setPending(data); return }
    connect.mutate(data)
  })

  return (
    <>
      <form onSubmit={onSubmit} className="space-y-3">
        <div>
          <Label>Secret key</Label>
          <Input
            type="password"
            autoComplete="off"
            placeholder={livemode ? 'sk_live_...' : 'sk_test_...'}
            {...register('secret_key')}
            className={cn('mt-1 font-mono', errors.secret_key && 'border-destructive')}
          />
          {errors.secret_key && <p className="text-xs text-destructive mt-1">{errors.secret_key.message}</p>}
          <p className="text-xs text-muted-foreground mt-1">
            Find it at{' '}
            <a
              href={livemode ? 'https://dashboard.stripe.com/apikeys' : 'https://dashboard.stripe.com/test/apikeys'}
              target="_blank"
              rel="noreferrer"
              className="underline underline-offset-2 hover:text-foreground inline-flex items-center gap-1"
            >
              dashboard.stripe.com/{livemode ? '' : 'test/'}apikeys
              <ExternalLink size={10} />
            </a>
            . Restricted keys (rk_) are also accepted.
          </p>
        </div>
        <div>
          <Label>Publishable key</Label>
          <Input
            type="text"
            autoComplete="off"
            placeholder={livemode ? 'pk_live_...' : 'pk_test_...'}
            {...register('publishable_key')}
            className={cn('mt-1 font-mono', errors.publishable_key && 'border-destructive')}
          />
          {errors.publishable_key && <p className="text-xs text-destructive mt-1">{errors.publishable_key.message}</p>}
        </div>
        <div className="flex items-center gap-2 pt-1">
          <Button type="submit" disabled={connect.isPending}>
            {connect.isPending ? (
              <><Loader2 size={14} className="animate-spin mr-2" /> Verifying with Stripe...</>
            ) : connected ? (
              'Rotate API keys'
            ) : (
              `Connect ${livemode ? 'live' : 'test'} mode`
            )}
          </Button>
        </div>
      </form>

      <AlertDialog open={!!pending} onOpenChange={(v) => { if (!v) setPending(null) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Rotate {livemode ? 'live' : 'test'} mode API keys?</AlertDialogTitle>
            <AlertDialogDescription>
              The current secret and publishable keys will stop working as soon as the new ones are accepted.
              Any in-flight charges or checkout scripts still using the old publishable key will fail until they reload.
              Stripe best practice is to roll the key in your Stripe Dashboard and paste the new one here in the same session.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (pending) connect.mutate(pending)
                setPending(null)
              }}
            >
              Rotate keys
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  )
}

// Step 2 body: shows the Velox webhook URL + event checklist, and a field
// to paste the resulting whsec_ after the tenant registers the endpoint in
// Stripe. Same component handles rotation from Manage mode (guarded by the
// confirmation dialog because Stripe signs with the new secret immediately).
function WebhookSetup({ current, onSuccess }: {
  current: StripeProviderCredentials
  onSuccess: () => void
}) {
  const methods = useForm<WebhookSecretForm>({
    resolver: zodResolver(webhookSecretSchema),
    defaultValues: { webhook_secret: '' },
  })
  const rotating = !!current.has_webhook_secret
  const [pending, setPending] = useState<WebhookSecretForm | null>(null)

  const save = useMutation({
    mutationFn: (data: WebhookSecretForm) =>
      api.setStripeWebhookSecret(current.livemode ? 'live' : 'test', data.webhook_secret),
    onSuccess: () => {
      toast.success(rotating ? 'Webhook secret replaced' : 'Webhook secret saved')
      methods.reset()
      onSuccess()
    },
    onError: (err) => {
      applyApiError(methods, err, ['webhook_secret'], {
        toastTitle: 'Failed to save webhook secret',
      })
    },
  })

  const onSubmit = methods.handleSubmit((data) => {
    if (rotating) { setPending(data); return }
    save.mutate(data)
  })

  return (
    <div className="space-y-4">
      <div className="p-3 rounded-lg border border-border bg-muted/30 space-y-2">
        <p className="text-xs text-muted-foreground">
          <strong className="text-foreground">A.</strong> In{' '}
          <a
            href={current.livemode ? 'https://dashboard.stripe.com/webhooks' : 'https://dashboard.stripe.com/test/webhooks'}
            target="_blank"
            rel="noreferrer"
            className="underline underline-offset-2 hover:text-foreground inline-flex items-center gap-1"
          >
            Stripe Dashboard → Developers → Webhooks
            <ExternalLink size={10} />
          </a>
          , click <em>Add endpoint</em> and paste this URL:
        </p>
        <WebhookUrlRow endpointId={current.id} />
        <p className="text-xs text-muted-foreground pt-1">Subscribe to these events:</p>
        <ul className="space-y-0.5 pl-1">
          {STRIPE_WEBHOOK_EVENTS.map(e => (
            <li key={e.name} className="flex items-baseline gap-2 text-xs">
              <span className="text-muted-foreground/60">•</span>
              <span className="min-w-0">
                <code className="font-mono text-foreground/90">{e.name}</code>
                <span className="text-muted-foreground"> — {e.purpose}</span>
              </span>
            </li>
          ))}
        </ul>
      </div>

      <form onSubmit={onSubmit} className="space-y-2">
        <Label className="text-xs text-foreground">
          <strong className="text-foreground">B.</strong> Paste the signing secret Stripe shows after saving the endpoint
        </Label>
        <Input
          type="password"
          autoComplete="off"
          placeholder="whsec_..."
          {...methods.register('webhook_secret')}
          className={cn('font-mono', methods.formState.errors.webhook_secret && 'border-destructive')}
        />
        {methods.formState.errors.webhook_secret && (
          <p className="text-xs text-destructive">{methods.formState.errors.webhook_secret.message}</p>
        )}
        <div className="flex items-center gap-2 pt-1">
          <Button type="submit" size="sm" disabled={save.isPending}>
            {save.isPending ? (
              <><Loader2 size={14} className="animate-spin mr-2" /> Saving...</>
            ) : rotating ? (
              'Replace webhook secret'
            ) : (
              'Finish setup'
            )}
          </Button>
        </div>
      </form>

      <AlertDialog open={!!pending} onOpenChange={(v) => { if (!v) setPending(null) }}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>Replace webhook signing secret?</AlertDialogTitle>
            <AlertDialogDescription>
              Stripe will sign webhook deliveries with the new secret immediately. Events still in
              flight under the old secret will fail signature verification and retry on Stripe's schedule.
              Roll the secret in Stripe Dashboard first, then paste the new one here.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                if (pending) save.mutate(pending)
                setPending(null)
              }}
            >
              Replace secret
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  )
}

function DisconnectAction({ mode, onDone }: { mode: 'test' | 'live'; onDone: () => void }) {
  const remove = useMutation({
    mutationFn: () => api.deleteStripeCredentials(mode),
    onSuccess: () => {
      toast.success(`Disconnected ${mode} mode`)
      onDone()
    },
    onError: (err) => {
      showApiError(err, 'Failed to disconnect')
    },
  })

  return (
    <AlertDialog>
      <AlertDialogTrigger render={<Button variant="ghost" size="sm" className="text-destructive hover:text-destructive" />}>
        <Trash2 size={14} className="mr-2" /> Disconnect {mode} mode
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Disconnect {mode} mode?</AlertDialogTitle>
          <AlertDialogDescription>
            Velox will stop charging new invoices through this Stripe account.
            Existing Stripe charges and refunds are unaffected. You can reconnect with new keys anytime.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction onClick={() => remove.mutate()}>Disconnect</AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}

// Stripe-dashboard-style masked display: "sk_live_51ab••••••••wxyz".
// Prefix (from DB) is the key type + a few account-identifying chars — lets
// the tenant match what they see in their Stripe dashboard at a glance.
// Falls back to "•••• {last4}" for rows created before 0033 (prefix empty).
function formatSecretKey(prefix: string | undefined, last4: string | undefined): string {
  if (!last4) return '—'
  if (prefix) return `${prefix}••••••••${last4}`
  return `•••• ${last4}`
}

function ReadOnlyRow({ label, value, mono, action }: {
  label: string
  value: string
  mono?: boolean
  action?: React.ReactNode
}) {
  return (
    <div className="py-2.5 px-3 bg-muted rounded-lg">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <p className="text-xs text-muted-foreground mb-0.5">{label}</p>
          <p className={cn('text-sm text-foreground break-all', mono && 'font-mono')}>{value}</p>
        </div>
        {action && <div className="shrink-0">{action}</div>}
      </div>
    </div>
  )
}

// Just the URL + copy. Event subscription list lives in WebhookSetup (only
// relevant during setup/rotation, not when reviewing a connected integration).
function WebhookUrlRow({ endpointId }: { endpointId: string }) {
  const url = `${API_ORIGIN}/v1/webhooks/stripe/${endpointId}`
  return (
    <div className="py-2.5 px-3 bg-background border border-border rounded-lg">
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0 flex-1">
          <p className="text-[11px] uppercase tracking-wide text-muted-foreground mb-0.5">
            Webhook URL
          </p>
          <code className="text-xs font-mono text-foreground break-all">{url}</code>
        </div>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => { navigator.clipboard.writeText(url); toast.success('Webhook URL copied') }}
        >
          <Copy size={14} />
        </Button>
      </div>
    </div>
  )
}
