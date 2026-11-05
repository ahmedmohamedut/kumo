import type { StatusIndicatorProps } from '@cloudscape-design/components'

import type { DBInstance, DBInstanceState } from '@/features/dbinstances/api'

/**
 * State → StatusIndicator mapping for DB instances (03-frontend.md §6.9).
 *
 * The states are the database's, not the server's: `available` means the
 * engine answered on the VPC address, and a guest that is running with an
 * engine that never answered reads as `failed`.
 */
export const DB_INSTANCE_INDICATOR: Record<
  DBInstanceState,
  { type: StatusIndicatorProps.Type; label: string }
> = {
  creating: { type: 'in-progress', label: 'Creating' },
  available: { type: 'success', label: 'Available' },
  starting: { type: 'in-progress', label: 'Starting' },
  stopping: { type: 'in-progress', label: 'Stopping' },
  stopped: { type: 'stopped', label: 'Stopped' },
  rebooting: { type: 'in-progress', label: 'Rebooting' },
  deleting: { type: 'in-progress', label: 'Deleting' },
  failed: { type: 'error', label: 'Failed' },
}

/** Bounds of the create form, from 01-api.md §Databases. */
export const MAX_IDENTIFIER_LENGTH = 60
export const MAX_NAME_LENGTH = 63
export const MIN_PASSWORD_LENGTH = 10
export const MAX_PASSWORD_LENGTH = 128
export const MIN_STORAGE_GB = 5
export const MAX_STORAGE_GB = 2048

/** The identifier is a DNS label: it becomes the endpoint's first segment. */
const IDENTIFIER_RE = /^[a-z]([-a-z0-9]*[a-z0-9])?$/

/** The admin role and the initial database are unquoted Postgres identifiers. */
const PG_NAME_RE = /^[a-z_][a-z0-9_]*$/

/**
 * Names Postgres or Kumo keeps for itself. The API refuses them, and so does
 * the form, so the red field arrives before the round trip.
 */
const RESERVED = new Set(['postgres', 'template0', 'template1', 'rdsadmin', 'kumo'])

/** identifierProblem explains why an identifier is refused locally, or null. */
export function identifierProblem(identifier: string): string | null {
  if (identifier === '') {
    return 'Enter an identifier for the database.'
  }
  if (identifier.length > MAX_IDENTIFIER_LENGTH) {
    return `Must be at most ${MAX_IDENTIFIER_LENGTH} characters.`
  }
  if (!IDENTIFIER_RE.test(identifier) || identifier.includes('--')) {
    return 'Must start with a letter and hold lowercase letters, digits and single dashes, and cannot end with a dash.'
  }
  return null
}

/** pgNameProblem explains why a role or database name is refused, or null. */
export function pgNameProblem(name: string, what: string): string | null {
  if (name === '') {
    return `Enter ${what}.`
  }
  if (name.length > MAX_NAME_LENGTH) {
    return `Must be at most ${MAX_NAME_LENGTH} characters.`
  }
  if (!PG_NAME_RE.test(name)) {
    return 'Must start with a lowercase letter or underscore and hold only lowercase letters, digits and underscores.'
  }
  if (RESERVED.has(name) || name.startsWith('pg_')) {
    return `"${name}" is reserved by Postgres or by Kumo.`
  }
  return null
}

/**
 * passwordProblem explains why a password is refused locally, or null.
 *
 * The printable-ASCII rule is not a style preference: the engine derives its
 * credential from the password through SASLprep and Kumo does not, so anything
 * else would authenticate nowhere (01-api.md §Databases). Catching it here is
 * what keeps a user from setting a password that looks accepted and then fails
 * at the first connection.
 */
export function passwordProblem(password: string): string | null {
  if (password.length < MIN_PASSWORD_LENGTH) {
    return `Must be at least ${MIN_PASSWORD_LENGTH} characters.`
  }
  if (password.length > MAX_PASSWORD_LENGTH) {
    return `Must be at most ${MAX_PASSWORD_LENGTH} characters.`
  }
  for (const character of password) {
    const code = character.codePointAt(0) ?? 0
    if (code < 0x20 || code > 0x7e) {
      return 'Must hold printable ASCII only. The engine derives its credential from the password before Kumo can normalize it, so any other character would stop the password working.'
    }
  }
  return null
}

/** storageProblem bounds the data disk. */
export function storageProblem(storageGB: number): string | null {
  if (!Number.isInteger(storageGB) || storageGB < MIN_STORAGE_GB || storageGB > MAX_STORAGE_GB) {
    return `Must be a whole number between ${MIN_STORAGE_GB} and ${MAX_STORAGE_GB} GiB.`
  }
  return null
}

/**
 * psqlExample is the command the Connect panel offers. The password is not in
 * it, and there is no way to put it there: Kumo discarded it when the database
 * was created, which is what `psql` prompting for it says out loud.
 */
export function psqlExample(db: DBInstance): string {
  const host = db.endpoint ?? '<endpoint>'
  return `psql "host=${host} port=${db.port} dbname=${db.database_name} user=${db.admin_username}"`
}

/** connectionURI is the same connection as a libpq URI, for a config file. */
export function connectionURI(db: DBInstance): string {
  const host = db.endpoint ?? '<endpoint>'
  return `postgresql://${db.admin_username}@${host}:${db.port}/${db.database_name}`
}

/** canStop, canStart and canReboot are the state machine of 01-api.md. */
export function canStop(state: DBInstanceState): boolean {
  return state === 'available'
}

export function canStart(state: DBInstanceState): boolean {
  return state === 'stopped'
}

export function canReboot(state: DBInstanceState): boolean {
  return state === 'available'
}

/** canDelete: every state but `deleting`, because delete is also the way out. */
export function canDelete(state: DBInstanceState): boolean {
  return state !== 'deleting'
}
