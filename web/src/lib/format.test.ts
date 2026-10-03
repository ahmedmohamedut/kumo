import { describe, expect, it } from 'vitest'

import { formatAbsoluteTime, formatRelativeTime } from '@/lib/format'

/** The reference "now" every case below is measured against. */
const NOW = new Date('2026-03-15T12:00:00Z')

/** ago renders the instant `seconds` before NOW, as the API would send it. */
function ago(seconds: number): string {
  return new Date(NOW.getTime() - seconds * 1000).toISOString()
}

describe('formatRelativeTime', () => {
  const cases: ReadonlyArray<[string, string, string]> = [
    ['just now', ago(0), 'just now'],
    ['seconds read as just now', ago(45), 'just now'],
    ['one minute', ago(60), '1 minute ago'],
    ['minutes', ago(15 * 60), '15 minutes ago'],
    ['one hour', ago(3600), '1 hour ago'],
    ['hours', ago(5 * 3600), '5 hours ago'],
    ['one day', ago(24 * 3600), '1 day ago'],
    ['days', ago(9 * 24 * 3600), '9 days ago'],
    ['a month past 30 days', ago(31 * 24 * 3600), '1 month ago'],
    ['months', ago(200 * 24 * 3600), '6 months ago'],
    ['a year', ago(400 * 24 * 3600), '1 year ago'],
  ]

  it.each(cases)('%s', (_label, value, want) => {
    expect(formatRelativeTime(value, NOW)).toBe(want)
  })

  it('reads a timestamp in the future as just now, not a negative age', () => {
    // The console compares against the client clock, which drifts; a negative
    // age would be nonsense to show an operator.
    const ahead = new Date(NOW.getTime() + 90 * 1000).toISOString()
    expect(formatRelativeTime(ahead, NOW)).toBe('just now')
  })

  it('marks an unparseable timestamp rather than rendering NaN', () => {
    expect(formatRelativeTime('not a date', NOW)).toBe('—')
  })
})

describe('formatAbsoluteTime', () => {
  it('renders UTC, matching what the API stored', () => {
    expect(formatAbsoluteTime('2026-01-02T09:30:00Z')).toBe('2026-01-02 09:30:00 UTC')
  })

  it('marks an unparseable timestamp', () => {
    expect(formatAbsoluteTime('')).toBe('—')
  })
})
