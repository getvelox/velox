import * as React from 'react'
import { Popover as PopoverPrimitive } from '@base-ui/react/popover'
import { Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList } from '@/components/ui/command'
import { ChevronsUpDown, Check, X } from 'lucide-react'
import { cn } from '@/lib/utils'

export interface ComboboxOption {
  value: string
  label: string
  // Optional search helpers (e.g. include ISO code + local name + english name).
  keywords?: string[]
  // Optional leading visual (flag emoji, icon, currency symbol).
  prefix?: React.ReactNode
}

interface ComboboxProps {
  value: string
  onChange: (value: string) => void
  options: ComboboxOption[]
  placeholder?: string
  emptyMessage?: string
  className?: string
  triggerClassName?: string
  disabled?: boolean
  clearable?: boolean
  // Override the display in the trigger; defaults to selected option's label.
  renderSelected?: (opt: ComboboxOption | undefined) => React.ReactNode
  // Server-side search mode: the parent owns filtering. cmdk's internal
  // matcher is disabled (every option shown as-is) and each keystroke is
  // reported so the parent can refetch. Without this, any picker backed
  // by a paginated list can only ever match within the first page —
  // rows past the page cap are unfindable no matter what the user types.
  onSearchChange?: (search: string) => void
  serverFiltered?: boolean
  // Let the popup grow past the trigger (up to 32rem, still clamped by the
  // viewport) for pickers with long labels that are NOT inside a dialog.
  // Default false: popup == trigger, which is the only width that is
  // guaranteed contained wherever the trigger sits. See the Popup style.
  growToFit?: boolean
}

// Searchable dropdown built on cmdk, mounted through Base UI's Popover
// (Portal + Positioner) — the SAME primitive as DatePicker/Select. The popup
// is portaled to the body, so an ancestor Card's `overflow-hidden` (every Card
// clips for its rounded corners) can no longer cut the top/bottom rows off —
// the clipping bug the old hand-rolled `absolute` popover had. Base UI also
// owns anchoring, flip-when-tight, edge-alignment, available-height, and
// outside-press/Escape dismissal, which we used to compute by hand.
export function Combobox({
  value,
  onChange,
  options,
  placeholder = 'Select...',
  emptyMessage = 'No results',
  className,
  triggerClassName,
  disabled,
  clearable,
  renderSelected,
  onSearchChange,
  serverFiltered,
  growToFit,
}: ComboboxProps) {
  const [open, setOpen] = React.useState(false)
  const anchorRef = React.useRef<HTMLDivElement>(null)

  // Dedupe options by value defensively. CommandItem keys on
  // opt.value; a caller that hands us duplicates (e.g. tzdb listing
  // the same canonical zone twice) blows up React with a duplicate-
  // key warning and renders the same row twice. Map preserves the
  // first occurrence of each value, which is the conventional pick
  // for sorted lists.
  const dedupedOptions = React.useMemo(() => {
    const seen = new Map<string, ComboboxOption>()
    for (const o of options) {
      if (!seen.has(o.value)) seen.set(o.value, o)
    }
    return Array.from(seen.values())
  }, [options])

  const selected = dedupedOptions.find(o => o.value === value)

  const handleSelect = (val: string) => {
    onChange(val)
    setOpen(false)
  }

  const handleClear = (e: React.MouseEvent) => {
    e.stopPropagation()
    onChange('')
  }

  return (
    <PopoverPrimitive.Root open={open} onOpenChange={setOpen}>
      <div ref={anchorRef} className={cn('w-full', className)}>
        <PopoverPrimitive.Trigger
          render={
            <button
              type="button"
              disabled={disabled}
              className={cn(
                'w-full flex items-center justify-between gap-2 rounded-md border border-input bg-background px-3 py-2 text-sm text-foreground',
                'ring-offset-background focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2',
                'disabled:cursor-not-allowed disabled:opacity-50',
                !selected && 'text-muted-foreground',
                triggerClassName,
              )}
              aria-haspopup="listbox"
            >
              <span className="flex items-center gap-2 min-w-0 truncate">
                {selected?.prefix}
                <span className="truncate">
                  {renderSelected ? renderSelected(selected) : (selected?.label ?? placeholder)}
                </span>
              </span>
              <span className="flex items-center gap-1 shrink-0">
                {clearable && selected && !disabled && (
                  <span
                    role="button"
                    tabIndex={-1}
                    aria-label="Clear selection"
                    onClick={handleClear}
                    className="h-5 w-5 rounded text-muted-foreground hover:text-foreground hover:bg-accent flex items-center justify-center"
                  >
                    <X size={12} />
                  </span>
                )}
                <ChevronsUpDown size={14} className="text-muted-foreground" />
              </span>
            </button>
          }
        />
      </div>

      <PopoverPrimitive.Portal>
        <PopoverPrimitive.Positioner
          anchor={anchorRef}
          side="bottom"
          align="start"
          sideOffset={4}
          className="isolate z-50 outline-none"
        >
          <PopoverPrimitive.Popup
            // Width MATCHES THE TRIGGER exactly (found 2026-07-30 walking the
            // Change Plan dialog): the popup used to grow to 32rem bounded only
            // by `--available-width`, which is room to the VIEWPORT — not to the
            // dialog the trigger usually lives in. So inside any dialog narrower
            // than 32rem the list spilled past both dialog edges and covered the
            // controls beneath it, while STILL truncating a long option label.
            // Growing bought nothing there and looked broken, so the safe shape
            // is the predictable one (shadcn/Radix Select do the same): popup ==
            // trigger, labels truncate with the full text in a title tooltip.
            // `growToFit` opts a NON-dialog combobox back into growing, and is
            // still clamped by --available-width.
            //
            // max-height stays bounded by the room Base UI measured on whichever
            // side it flipped to, so the list scrolls instead of overflowing —
            // and the Portal means no ancestor can clip it.
            style={{
              minWidth: 'var(--anchor-width)',
              maxWidth: growToFit
                ? 'min(32rem, var(--available-width))'
                : 'var(--anchor-width)',
              maxHeight: 'var(--available-height)',
            }}
            className={cn(
              'flex flex-col overflow-hidden rounded-md border border-border bg-popover text-popover-foreground shadow-md outline-none',
              'data-open:animate-in data-open:fade-in-0 data-open:zoom-in-95 data-closed:animate-out data-closed:fade-out-0 data-closed:zoom-out-95',
            )}
          >
            <Command
              shouldFilter={!serverFiltered}
              filter={(val, search, keywords) => {
                const haystack = [val, ...(keywords ?? [])].join(' ').toLowerCase()
                return haystack.includes(search.toLowerCase()) ? 1 : 0
              }}
              className="flex flex-1 flex-col min-h-0"
            >
              <CommandInput placeholder="Search..." autoFocus onValueChange={onSearchChange} />
              {/* CommandList scrolls within the remaining vertical space
                  after the search input. flex-1 + min-h-0 is the standard
                  pattern to let a flex child actually shrink below its
                  content height. */}
              <CommandList className="flex-1 min-h-0 overflow-y-auto">
                <CommandEmpty>{emptyMessage}</CommandEmpty>
                <CommandGroup>
                  {dedupedOptions.map(opt => (
                    <CommandItem
                      key={opt.value}
                      value={opt.label}
                      keywords={[opt.value, ...(opt.keywords ?? [])]}
                      onSelect={() => handleSelect(opt.value)}
                    >
                      {/* Truncation honesty (2026-07-30): the popup is trigger-
                          width, so long labels DO clip — the full value must stay
                          reachable rather than being silently lost to an ellipsis. */}
                      <span className="flex items-center gap-2 min-w-0 flex-1 truncate" title={opt.label}>
                        {opt.prefix}
                        <span className="truncate">{opt.label}</span>
                      </span>
                      <Check
                        size={14}
                        className={cn('ml-2', value === opt.value ? 'opacity-100' : 'opacity-0')}
                      />
                    </CommandItem>
                  ))}
                </CommandGroup>
              </CommandList>
            </Command>
          </PopoverPrimitive.Popup>
        </PopoverPrimitive.Positioner>
      </PopoverPrimitive.Portal>
    </PopoverPrimitive.Root>
  )
}
