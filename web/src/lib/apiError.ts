import type { SerializedError } from '@reduxjs/toolkit'
import type { FetchBaseQueryError } from '@reduxjs/toolkit/query'

import type { components } from '@/types/api'

type ErrorResponse = components['schemas']['ErrorResponse']

/**
 * ApiError is what an RTK Query endpoint can fail with: a baseQuery error, or a
 * serialization error for a response the console cannot read.
 */
export type ApiError = FetchBaseQueryError | SerializedError | undefined

/**
 * envelopeOf pulls the API envelope out of a failure.
 *
 * Every non-2xx from kumod carries `{error: {code, message, details}}`
 * (01-api.md), but a request can also fail before it reaches it: offline, a
 * CORS rejection, an aborted request, an unreadable body. Those have no
 * envelope, which is why every helper here takes a fallback message.
 */
export function envelopeOf(error: ApiError): ErrorResponse | null {
  const data: unknown = (error as { data?: unknown } | undefined)?.data
  if (typeof data !== 'object' || data === null || !('error' in data)) {
    return null
  }
  const envelope = (data as ErrorResponse).error
  return typeof envelope?.message === 'string' ? (data as ErrorResponse) : null
}

/** errorMessage is the summary to show in an Alert. */
export function errorMessage(error: ApiError, fallback: string): string {
  const envelope = envelopeOf(error)
  if (envelope !== null) {
    return envelope.error.message
  }
  if ((error as FetchBaseQueryError | undefined)?.status === 'FETCH_ERROR') {
    return 'kumo is unreachable. Check the network and try again.'
  }
  return fallback
}

/**
 * isNotFound reports whether the failure is a `404` from the API.
 *
 * A `404` is an answer rather than a fault: the API is saying the resource is
 * gone, not that something broke. It matters for instances, because a terminate
 * deletes the row — a page left open outlives the instance it is showing, and
 * telling an operator that the instance they removed "could not be loaded" is
 * true in the least helpful way available.
 */
export function isNotFound(error: ApiError): boolean {
  return (error as FetchBaseQueryError | undefined)?.status === 404
}

/**
 * fieldErrors maps `details[].field` onto the FormField that caused it, which
 * is what 03-frontend.md §7 asks for: a validation failure lands on the field
 * instead of in a banner above the form.
 */
export function fieldErrors(error: ApiError): Record<string, string> {
  const details = envelopeOf(error)?.error.details ?? []
  const errors: Record<string, string> = {}
  for (const detail of details) {
    errors[detail.field] = detail.reason
  }
  return errors
}
