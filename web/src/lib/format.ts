/**
 * Formatters shared by the console (03-frontend.md §7).
 *
 * Every function takes the timestamp as the API sends it — an RFC3339 string —
 * and returns something to put in a cell, so the table code never parses dates
 * itself. `now` is a parameter rather than a call to `Date.now()` so a test can
 * pin the clock instead of asserting against a moving target.
 */

/** Units a relative time is rounded to, coarsest last. */
const MINUTE = 60
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

/**
 * formatRelativeTime renders how long ago something happened, e.g. "3 minutes
 * ago". Anything in the future reads as "just now": the console compares against
 * the client clock, which drifts, and a negative age would be a strange thing to
 * show an operator.
 */
export function formatRelativeTime(value: string, now: Date = new Date()): string {
  const then = new Date(value)
  if (Number.isNaN(then.getTime())) {
    return '—'
  }

  const seconds = Math.floor((now.getTime() - then.getTime()) / 1000)
  if (seconds < MINUTE) {
    return 'just now'
  }

  const [magnitude, unit] = describe(seconds)
  return `${magnitude} ${unit}${magnitude === 1 ? '' : 's'} ago`
}

/** describe splits an age in seconds into the largest whole unit that fits. */
function describe(seconds: number): [number, string] {
  if (seconds < HOUR) {
    return [Math.floor(seconds / MINUTE), 'minute']
  }
  if (seconds < DAY) {
    return [Math.floor(seconds / HOUR), 'hour']
  }
  if (seconds < 30 * DAY) {
    return [Math.floor(seconds / DAY), 'day']
  }
  // Past a month, a day count stops meaning anything; a calendar month does.
  const months = Math.floor(seconds / (30 * DAY))
  if (months < 12) {
    return [months, 'month']
  }
  return [Math.floor(seconds / (365 * DAY)), 'year']
}

/**
 * formatAbsoluteTime is the UTC timestamp shown in the tooltip behind a
 * relative one. UTC because that is what the API stores and renders
 * (AGENTS.md, backend conventions), so it matches the audit trail exactly.
 */
export function formatAbsoluteTime(value: string): string {
  const parsed = new Date(value)
  if (Number.isNaN(parsed.getTime())) {
    return '—'
  }
  return parsed
    .toISOString()
    .replace('T', ' ')
    .replace(/\.\d+Z$/, ' UTC')
}
