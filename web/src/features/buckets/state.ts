import type { StatusIndicatorProps } from '@cloudscape-design/components'

import type { Bucket, BucketDetail } from '@/features/buckets/api'

export type BucketState = Bucket['state']
export type Versioning = BucketDetail['versioning']

/** State → StatusIndicator mapping for buckets (03-frontend.md §4). */
export const BUCKET_INDICATOR: Record<
  BucketState,
  { type: StatusIndicatorProps.Type; label: string }
> = {
  available: { type: 'success', label: 'Available' },
  deleting: { type: 'in-progress', label: 'Deleting' },
}

/** VERSIONING_LABEL is how the console names a versioning state. */
export const VERSIONING_LABEL: Record<Versioning, string> = {
  off: 'Disabled',
  enabled: 'Enabled',
  suspended: 'Suspended',
}

/**
 * BUCKET_NAME_RE is the character rule of an S3 bucket name. The API also
 * refuses `..`, IP addresses and the reserved prefixes and suffixes; the form
 * checks the common cases so the red field arrives before the round trip.
 */
export const BUCKET_NAME_RE = /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/

/** bucketNameProblem explains why a name is refused locally, or returns null. */
export function bucketNameProblem(name: string): string | null {
  if (name === '') {
    return 'Enter a name for the bucket.'
  }
  if (!BUCKET_NAME_RE.test(name) || name.includes('..') || /^\d+\.\d+\.\d+\.\d+$/.test(name)) {
    return 'Must be 3-63 lowercase letters, digits, dots or dashes, start and end with a letter or digit, and not look like an IP address.'
  }
  return null
}
