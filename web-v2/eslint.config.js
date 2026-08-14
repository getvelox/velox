import js from '@eslint/js'
import globals from 'globals'
import reactHooks from 'eslint-plugin-react-hooks'
import reactRefresh from 'eslint-plugin-react-refresh'
import tseslint from 'typescript-eslint'
import { defineConfig, globalIgnores } from 'eslint/config'

// Relative-time gate (ADR-086 Phase 4): raw `Date.now()` / argless `new Date()`
// is wall-clock now — on a clock-pinned (test-clock-simulated) entity that's a
// lie. All age/countdown/window UI must anchor on an EffectiveNow built via
// effectiveNow(frozenISO) or wallClockNow() (src/lib/effectiveNow.ts), which the
// type system already makes non-optional at the helper boundary. This rule is
// the second, independent gate against hand-rolled wall-clock relative-time,
// and since 2026-07-19 it is a real CI gate: the frontend job runs
// `npm run lint`, and this rule is error-level so a violation fails CI.
// Genuine calendar / date-picker / infra uses opt out with a one-line
// eslint-disable + reason, or live in the date-infra modules exempted
// below.
const noWallClockNow = [
  'error',
  {
    selector: "CallExpression[callee.object.name='Date'][callee.property.name='now']",
    message:
      'Raw Date.now() ignores test clocks. Use effectiveNow(frozenISO) or wallClockNow() from @/lib/effectiveNow for relative-time. For genuine wall-clock/calendar/infra use, add `// eslint-disable-next-line no-restricted-syntax -- <reason>`.',
  },
  {
    selector: "NewExpression[callee.name='Date'][arguments.length=0]",
    message:
      'Argless `new Date()` is wall-clock now. Use effectiveNow()/wallClockNow() from @/lib/effectiveNow for relative-time. For calendar/date-picker/infra use, add `// eslint-disable-next-line no-restricted-syntax -- <reason>` (or keep it in lib/dates.ts).',
  },
]

// Scroll-affordance gate (2026-08-03). macOS overlay scrollbars are invisible
// until the user is already scrolling, so a capped pane whose content exceeds
// the cap renders IDENTICALLY to one whose content ended there. Nothing looks
// broken; the hidden rows are simply never discovered. The sidebar hid five
// entries including Settings at a laptop window height, and an operator reading
// a screenshot concluded those pages did not exist.
//
// The fix is <ScrollPane> (components/ui/scroll-pane.tsx), which fades whichever
// edge has content beyond it. This rule stops the next hand-rolled pane from
// silently reintroducing the defect — the failure is invisible to whoever
// writes it, which is exactly the kind that needs a machine to catch.
//
// Menu-shaped surfaces are exempted below: scrolling a dropdown is a universally
// understood affordance, they carry keyboard navigation that reveals the rest,
// and they are vendored primitives where divergence costs more than it buys.
const noRawScrollPane = {
  selector: "JSXAttribute[name.name='className'] Literal[value=/overflow-(y-)?(auto|scroll)/]",
  message:
    'A hand-rolled scroll pane gives no sign there is more content (macOS hides scrollbars until you scroll). Use <ScrollPane> from @/components/ui/scroll-pane, which fades the edge that has more. If this genuinely is a menu/vendored primitive, add `// eslint-disable-next-line no-restricted-syntax -- <reason>`.',
}

export default defineConfig([
  globalIgnores(['dist']),
  {
    files: ['**/*.{ts,tsx}'],
    extends: [
      js.configs.recommended,
      tseslint.configs.recommended,
      reactHooks.configs.flat.recommended,
      reactRefresh.configs.vite,
    ],
    languageOptions: {
      ecmaVersion: 2020,
      globals: globals.browser,
    },
    rules: {
      'no-restricted-syntax': [...noWallClockNow, noRawScrollPane],
      // HMR ergonomics only (fast-refresh works best when component files
      // export only components) — zero runtime correctness. shadcn-style
      // component files and context/hook co-location trip it ~40×; the
      // industry-standard posture for this template is warn, not error.
      'react-refresh/only-export-components': 'warn',
    },
  },
  {
    // Menu-shaped surfaces and the dialog shell. A dropdown/select/command
    // palette that scrolls is a universally understood affordance, they carry
    // keyboard navigation that reveals the rest, and they are vendored
    // primitives where divergence costs more than it buys. DialogContent scrolls
    // as one box including its own close button, so masking it would fade that
    // button — giving dialogs a proper scrolling BODY is a redesign of every
    // dialog in the app, deliberately not bundled into this fix.
    //
    // Layout.tsx is NOT here. It was, and that is how the app's largest pane —
    // <main>, where every page's content lives — kept a hand-rolled
    // `overflow-auto` while the rule was on. Nothing flagged it, because the
    // file had been waived wholesale for its menu-shaped parts. A blanket
    // file-level waiver is only as good as its narrowest member; anything in
    // Layout that genuinely needs one takes a line-level disable with a reason,
    // the same as every other caller.
    files: [
      'src/components/ui/dropdown-menu.tsx',
      'src/components/ui/select.tsx',
      'src/components/ui/command.tsx',
      'src/components/ui/dialog.tsx',
      'src/components/ui/scroll-pane.tsx',
      'src/components/Combobox.tsx',
    ],
    rules: {
      'no-restricted-syntax': noWallClockNow,
    },
  },
  {
    // Wall-clock IS the domain here: the date/timezone utility module and the
    // calendar picker component. Tests seed fixtures freely.
    files: ['src/lib/dates.ts', 'src/components/ui/date-picker.tsx', '**/*.test.{ts,tsx}'],
    rules: {
      'no-restricted-syntax': 'off',
    },
  },
])
