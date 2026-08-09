/**
 * The horizontal band every authed surface sits in.
 *
 * One constant rather than a repeated literal because multiple layers have
 * to agree on it — the page content, Settings' fixed save bar, and (mobile
 * only, since #768 removed the desktop top bar) the hamburger bar — and when
 * they disagree the misalignment is only visible on a wide monitor, which is
 * exactly where nobody develops.
 *
 * It was previously a bare `max-w-7xl mx-auto` written out at each site, and
 * they had already drifted: the top bar had no cap at all, so on a 2560px
 * viewport the Test/Live toggle sat at x=2544 while the page's own controls
 * ended at x=2040 — 504px apart, though they read as the same layer. Settings'
 * save bar carried a hand-copied duplicate of the literal, coupled to Layout by
 * nothing but the fact that someone typed the same characters.
 *
 * Padding deliberately stays with each caller: the content band wants p-4/p-8,
 * the top bar a tighter px-4 py-3, the save bar px-4/px-8. Only the WIDTH and
 * the centring are shared, because only those have to match.
 *
 * Note this is a max-width, not a width — below ~1300px viewports it has no
 * effect at all, which is why the drift was invisible for so long.
 */
// 1600, not 1280. The 1280 (max-w-7xl) arrived with the scaffold rather than
// from a decision, and it is a PROSE constant — Primer documents it as keeping
// "paragraphs with too many words per line". Velox's widest surfaces are
// ledgers, not paragraphs. 1600 is the nearest round number to Carbon's
// published 1584px grid cap, the one primary source above 1280 for dense
// content.
//
// Capped rather than full-bleed because the app has ZERO xl:/2xl: breakpoints —
// every grid finalises at lg: (1024), so above that width STRETCHES rather than
// reflowing. Uncapped at 2560 would give four ~570px KPI tiles and a ~2300x220
// revenue chart, a ~10:1 aspect that flattens the trend slope it exists to show.
//
// Nothing changes below a ~1520px viewport: at 1440 the available width is
// already under 1600, so the cap does not bind and every existing 1280-based
// assertion stays true by construction.
export const CONTENT_BAND = 'max-w-[1600px] mx-auto'
