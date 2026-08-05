import { useRef, useState, type ReactNode } from 'react'
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

export interface TypedConfirmDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description?: ReactNode
  /** Word the user must type verbatim to enable the confirm button. Matching is case-insensitive so caps lock doesn't trap the user. */
  confirmWord: string
  confirmLabel: string
  onConfirm: () => void
  loading?: boolean
  /**
   * Extra controls rendered between the description and the type-to-confirm
   * input — for a SECOND decision the same action needs (e.g. writing an
   * invoice off also asks whether to stop the subscription billing). Keep it
   * to choices; anything that needs its own confirmation belongs in its own
   * dialog.
   */
  children?: ReactNode
}

/**
 * Destructive-action confirmation that requires the user to type a specific
 * word before the confirm button enables. Use for truly irreversible financial
 * or state-changing actions (void invoice, cancel subscription, delete webhook
 * endpoint). For reversible actions (pause, archive) prefer a plain AlertDialog.
 */
export function TypedConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmWord,
  confirmLabel,
  onConfirm,
  loading = false,
  children,
}: TypedConfirmDialogProps) {
  const [typed, setTyped] = useState('')
  const inputRef = useRef<HTMLInputElement>(null)

  // Reset the typed text when the dialog closes, via the documented
  // adjust-state-during-render pattern (guarded compare, no effect):
  // an effect-based reset fires a render with the STALE text first and
  // covers only post-commit; this covers every close path — including a
  // parent flipping `open` programmatically after a confirm succeeds —
  // before anything visible renders.
  const [prevOpen, setPrevOpen] = useState(open)
  if (prevOpen !== open) {
    setPrevOpen(open)
    if (!open) setTyped('')
  }

  const matches = typed.trim().toUpperCase() === confirmWord.toUpperCase()

  return (
    <AlertDialog open={open} onOpenChange={onOpenChange}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          {description && <AlertDialogDescription>{description}</AlertDialogDescription>}
        </AlertDialogHeader>

        {children}

        <div className="space-y-2">
          <Label htmlFor="typed-confirm-input" className="text-sm">
            Type{' '}
            <span className="font-mono font-semibold text-foreground">{confirmWord}</span>{' '}
            to confirm
          </Label>
          <Input
            id="typed-confirm-input"
            ref={inputRef}
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            placeholder={confirmWord}
            autoComplete="off"
            autoCapitalize="characters"
            spellCheck={false}
            disabled={loading}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && matches && !loading) {
                e.preventDefault()
                onConfirm()
              }
            }}
          />
        </div>

        <AlertDialogFooter>
          <AlertDialogCancel disabled={loading}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            onClick={onConfirm}
            disabled={!matches || loading}
          >
            {confirmLabel}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
