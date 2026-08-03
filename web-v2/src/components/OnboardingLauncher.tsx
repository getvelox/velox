import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { Check, X, ChevronUp, ArrowRight } from 'lucide-react'
import { cn } from '@/lib/utils'
import { useOnboardingSteps, type OnboardingStep } from '@/hooks/useOnboardingSteps'
import { ScrollPane } from '@/components/ui/scroll-pane'

// OnboardingLauncher — floating bottom-right pill that expands into a
// 360px side panel. Mirrors the dominant 2026 dev-tool pattern (Intercom,
// Appcues, Chameleon defaults) rather than an in-page card. Self-hides when
// the user completes the checklist or dismisses it; state lives in the
// shared useOnboardingSteps hook.
export function OnboardingLauncher() {
  const [open, setOpen] = useState(false)
  const { steps, complete, total, show, setDismissed } = useOnboardingSteps()

  // Close on Escape while the panel is open. No focus-trap because the panel
  // is non-modal — it doesn't block page interaction.
  useEffect(() => {
    if (!open) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [open])

  if (!show) return null

  const next = steps.find(s => !s.done)

  if (!open) {
    return (
      <button
        type="button"
        onClick={() => setOpen(true)}
        aria-label={`Setup: ${complete} of ${total} complete. Open checklist.`}
        className="fixed bottom-5 right-5 z-30 flex items-center gap-2 rounded-full border border-border bg-card px-3 py-2 text-xs font-medium shadow-lg transition-all hover:border-primary/40 hover:shadow-xl"
      >
        <ProgressRing value={complete} max={total} />
        <span className="text-foreground">
          <span className="tabular-nums">{complete} of {total}</span>
          <span className="text-muted-foreground"> · Setup</span>
        </span>
        <ChevronUp size={13} className="text-muted-foreground" aria-hidden />
      </button>
    )
  }

  const percent = total > 0 ? (complete / total) * 100 : 0

  return (
    <div
      role="dialog"
      aria-labelledby="onboarding-launcher-title"
      className="fixed bottom-5 right-5 z-30 w-[360px] max-w-[calc(100vw-2.5rem)] rounded-xl border border-border bg-card shadow-2xl"
    >
      <div className="flex items-start gap-3 px-4 pt-4 pb-2">
        <div className="flex-1 min-w-0">
          <p id="onboarding-launcher-title" className="text-sm font-semibold text-foreground">
            Get Velox ready
          </p>
          <p className="mt-0.5 text-xs text-muted-foreground">
            <span className="tabular-nums">{complete} of {total}</span> complete
          </p>
        </div>
        <div className="flex shrink-0 items-center gap-1">
          <button
            type="button"
            onClick={() => setOpen(false)}
            aria-label="Collapse checklist"
            className="flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
          >
            <ChevronUp size={14} className="rotate-180" aria-hidden />
          </button>
          <button
            type="button"
            onClick={() => { setDismissed(); setOpen(false) }}
            aria-label="Hide setup checklist"
            className="flex h-7 w-7 items-center justify-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
          >
            <X size={14} aria-hidden />
          </button>
        </div>
      </div>

      <div className="px-4 pb-3">
        <div
          className="h-1 overflow-hidden rounded-full bg-muted"
          role="progressbar"
          aria-valuenow={complete}
          aria-valuemin={0}
          aria-valuemax={total}
          aria-label="Setup progress"
        >
          <div
            className="h-full rounded-full bg-primary transition-[width] duration-300"
            style={{ width: `${percent}%` }}
          />
        </div>
      </div>

      <ScrollPane as="ul" className="max-h-[60vh] border-t border-border">
        {steps.map((step) => (
          <StepRow
            key={step.key}
            step={step}
            isNext={step === next}
            onNavigate={() => setOpen(false)}
          />
        ))}
      </ScrollPane>

    </div>
  )
}

function StepRow({
  step, isNext, onNavigate,
}: {
  step: OnboardingStep; isNext: boolean; onNavigate: () => void
}) {
  const indicator = step.done ? (
    <span
      aria-label="done"
      className="flex h-5 w-5 shrink-0 items-center justify-center rounded-full bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
    >
      <Check size={12} aria-hidden />
    </span>
  ) : (
    <span
      aria-hidden
      className={cn(
        'flex h-5 w-5 shrink-0 items-center justify-center rounded-full border',
        isNext ? 'border-primary bg-primary/10' : 'border-border',
      )}
    >
      {isNext && <span className="h-1.5 w-1.5 rounded-full bg-primary" />}
    </span>
  )

  const body = (
    <div
      className={cn(
        'flex items-center gap-3 border-b border-border px-4 py-3 transition-colors last:border-b-0',
        !step.done && 'hover:bg-accent/40',
        isNext && 'bg-primary/[0.04]',
      )}
    >
      {indicator}
      <div className="min-w-0 flex-1">
        <p className={cn(
          'text-sm font-medium',
          step.done ? 'text-muted-foreground line-through decoration-muted-foreground/40' : 'text-foreground',
        )}>
          {step.label}
        </p>
        {isNext && (
          <p className="mt-0.5 text-xs text-muted-foreground">{step.desc}</p>
        )}
      </div>
      {isNext && (
        <span className="flex shrink-0 items-center gap-1 text-xs font-medium text-primary">
          {step.cta}
          <ArrowRight size={12} aria-hidden />
        </span>
      )}
    </div>
  )

  return !step.done ? (
    <li>
      <Link to={step.to} onClick={onNavigate}>{body}</Link>
    </li>
  ) : (
    <li>{body}</li>
  )
}

function ProgressRing({
  value, max, size = 18, stroke = 2,
}: {
  value: number; max: number; size?: number; stroke?: number
}) {
  const radius = (size - stroke) / 2
  const circumference = 2 * Math.PI * radius
  const progress = max > 0 ? value / max : 0
  const offset = circumference * (1 - progress)
  return (
    <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} className="shrink-0" aria-hidden>
      <circle
        cx={size / 2}
        cy={size / 2}
        r={radius}
        fill="none"
        strokeWidth={stroke}
        className="stroke-muted"
      />
      <circle
        cx={size / 2}
        cy={size / 2}
        r={radius}
        fill="none"
        strokeWidth={stroke}
        strokeLinecap="round"
        strokeDasharray={circumference}
        strokeDashoffset={offset}
        className="stroke-primary transition-[stroke-dashoffset] duration-300"
        transform={`rotate(-90 ${size / 2} ${size / 2})`}
      />
    </svg>
  )
}
