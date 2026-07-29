import { useState } from 'react'
import { usePageTitle } from '@/hooks/usePageTitle'
import { Link } from 'react-router-dom'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { zodResolver } from '@hookform/resolvers/zod'
import { z } from 'zod'
import { toast } from 'sonner'

import { api, formatDate, formatDateTime, getTenantTimezone, type TestClock } from '@/lib/api'
import { applyApiError, showApiError } from '@/lib/formErrors'
import { formatInTimeZone, fromZonedTime } from 'date-fns-tz'
import { Layout } from '@/components/Layout'
import { statusBadgeVariant } from '@/lib/status'
import { EmptyState } from '@/components/EmptyState'
import { CardListSkeleton } from '@/components/ui/TableSkeleton'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { DateTimePicker } from '@/components/ui/datetime-picker'
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import {
  Form, FormControl, FormField, FormItem, FormLabel, FormMessage,
} from '@/components/ui/form'
import { useAuth } from '@/contexts/AuthContext'
import { useNavigate } from 'react-router-dom'
import { useEffect } from 'react'
import { Plus, Clock as ClockIcon, Loader2 } from 'lucide-react'

const createSchema = z.object({
  name: z.string().max(200, 'Name must be at most 200 characters').optional(),
})
type CreateData = z.infer<typeof createSchema>

function statusBadge(status: TestClock['status']) {
  switch (status) {
    case 'ready':
      return <Badge variant={statusBadgeVariant('ready')}>Ready</Badge>
    case 'advancing':
      return (
        <Badge variant="outline" className="border-blue-500/30 bg-blue-500/10 text-blue-700 dark:text-blue-400">
          <Loader2 size={10} className="animate-spin mr-1" /> Advancing
        </Badge>
      )
    case 'internal_failure':
      return <Badge variant={statusBadgeVariant('failed')}>Failed</Badge>
  }
}

export default function TestClocksPage() {
  usePageTitle('Test clocks')
  const { user } = useAuth()
  const navigate = useNavigate()
  const [showCreate, setShowCreate] = useState(false)

  // Test clocks are test-mode-only. If the operator flips to live while
  // viewing this page, redirect home — same defensive guard the backend
  // enforces with requireTestMode.
  useEffect(() => {
    if (user?.livemode) navigate('/', { replace: true })
  }, [user?.livemode, navigate])

  const { data, isLoading, error, refetch } = useQuery({
    queryKey: ['test-clocks'],
    queryFn: () => api.listTestClocks(),
    enabled: !!user && !user.livemode,
  })
  const clocks = data?.data ?? []
  const errMsg = error instanceof Error ? error.message : null

  return (
    <Layout>
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Test Clocks</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Simulate time-bound billing without waiting for real days to pass.
            Test mode only.
          </p>
        </div>
        <Button size="sm" onClick={() => setShowCreate(true)}>
          <Plus size={16} className="mr-2" />
          New test clock
        </Button>
      </div>

      {errMsg ? (
        <Card className="mt-6">
          <CardContent className="p-8 text-center">
            <p className="text-sm text-destructive mb-3">{errMsg}</p>
            <Button variant="outline" size="sm" onClick={() => refetch()}>Retry</Button>
          </CardContent>
        </Card>
      ) : isLoading ? (
        <div className="mt-6">
          <CardListSkeleton rows={3} />
        </div>
      ) : clocks.length === 0 ? (
        <Card className="mt-6">
          <CardContent className="p-0">
            <EmptyState
              icon={ClockIcon}
              title="No test clocks yet"
              description="A test clock freezes 'now' for the subscriptions you pin to it. Advance the clock to fast-forward billing without waiting for real time to pass."
              action={{
                label: 'New test clock',
                icon: Plus,
                onClick: () => setShowCreate(true),
              }}
            />
          </CardContent>
        </Card>
      ) : (
        <div className="mt-6 space-y-3">
          {clocks.map(c => (
            <Link key={c.id} to={`/test-clocks/${c.id}`} className="block">
              <Card className="hover:bg-accent/30 transition-colors">
                <CardContent className="px-6 py-4 flex items-center gap-4">
                  <div className="h-9 w-9 rounded-lg bg-violet-50 dark:bg-violet-500/10 flex items-center justify-center shrink-0">
                    <ClockIcon size={16} className="text-violet-500" />
                  </div>
                  <div className="flex-1 min-w-0">
                    <div className="flex items-center gap-2">
                      <p className="text-sm font-medium text-foreground truncate">
                        {c.name || 'Unnamed clock'}
                      </p>
                      {statusBadge(c.status)}
                    </div>
                    <p className="text-xs text-muted-foreground mt-0.5">
                      Clock time: <span className="font-mono">{formatDateTime(c.frozen_time)}</span>
                    </p>
                  </div>
                  <p className="text-xs text-muted-foreground shrink-0">
                    Created {formatDate(c.created_at)}
                  </p>
                </CardContent>
              </Card>
            </Link>
          ))}
        </div>
      )}

      {showCreate && <CreateClockDialog onClose={() => setShowCreate(false)} />}
    </Layout>
  )
}

function CreateClockDialog({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  // ADR-010: every operator-picked civil datetime is interpreted in
  // tenant TZ. Date + time edited independently so the branded
  // DatePicker (consistent with API Keys / Coupons / Credits) handles
  // the date and a small text input handles HH:mm.
  const tz = getTenantTimezone() || 'UTC'
  // A brand-new clock's start defaults to the real present — genuinely wall-clock
  // (there is no simulated "now" yet; this is the moment the operator creates it).
  // eslint-disable-next-line no-restricted-syntax -- new-clock start defaults to real now
  const [datePart, setDatePart] = useState(() => formatInTimeZone(new Date(), tz, 'yyyy-MM-dd'))
  // eslint-disable-next-line no-restricted-syntax -- new-clock start defaults to real now
  const [timePart, setTimePart] = useState(() => formatInTimeZone(new Date(), tz, 'HH:mm'))
  const [pickerError, setPickerError] = useState('')

  const form = useForm<CreateData>({
    resolver: zodResolver(createSchema),
    defaultValues: { name: '' },
  })

  const onSubmit = form.handleSubmit(async data => {
    setPickerError('')
    if (!datePart) { setPickerError('Date is required'); return }
    if (!/^\d{2}:\d{2}$/.test(timePart)) { setPickerError('Time must be HH:MM'); return }
    try {
      // Re-ground "wall-clock in tenant TZ" → UTC ISO. Without this,
      // the browser would interpret the YYYY-MM-DDTHH:mm string as
      // local time and the resulting UTC ISO would skew by
      // (tenant_TZ_offset − browser_TZ_offset).
      const isoUtc = fromZonedTime(`${datePart}T${timePart}:00`, tz).toISOString()
      const clk = await api.createTestClock({
        name: data.name || '',
        frozen_time: isoUtc,
      })
      toast.success('Test clock created')
      queryClient.invalidateQueries({ queryKey: ['test-clocks'] })
      onClose()
      navigate(`/test-clocks/${clk.id}`)
    } catch (err) {
      applyApiError(form, err, ['name', 'frozen_time'], { toastTitle: 'Failed to create test clock' })
    }
  })

  return (
    <Dialog open onOpenChange={open => { if (!open) onClose() }}>
      <DialogContent className="sm:max-w-md">
        <DialogHeader>
          <DialogTitle>New test clock</DialogTitle>
          <DialogDescription>
            Pick the initial time for this simulator. Subscriptions you pin
            here will see this as "now" until you advance the clock.
          </DialogDescription>
        </DialogHeader>
        <Form {...form}>
          <form onSubmit={onSubmit} noValidate className="space-y-4">
            <FormField
              control={form.control}
              name="name"
              render={({ field }) => (
                <FormItem>
                  <FormLabel>Name (optional)</FormLabel>
                  <FormControl>
                    <Input placeholder="e.g. Annual renewal scenario" maxLength={200} {...field} />
                  </FormControl>
                  <FormMessage />
                </FormItem>
              )}
            />
            <div className="space-y-1.5">
              <Label className="text-sm font-medium">Initial clock time</Label>
              <DateTimePicker
                date={datePart}
                time={timePart}
                onDateChange={(d) => { setDatePart(d); setPickerError('') }}
                onTimeChange={(t) => { setTimePart(t); setPickerError('') }}
              />
              <p className="text-xs text-muted-foreground">
                Times are in your tenant timezone ({tz}). The clock starts here; use Advance from the detail page to move it forward.
              </p>
              {pickerError && <p className="text-xs text-destructive">{pickerError}</p>}
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={onClose}>Cancel</Button>
              <Button type="submit" disabled={form.formState.isSubmitting}>
                {form.formState.isSubmitting ? (
                  <><Loader2 size={14} className="animate-spin mr-2" />Creating…</>
                ) : 'Create test clock'}
              </Button>
            </DialogFooter>
          </form>
        </Form>
      </DialogContent>
    </Dialog>
  )
}

// silence unused-import eslint warning when nothing else surfaces it
void showApiError
