import { http, HttpResponse } from 'msw'

import type { components } from '@/types/api'

type AuthUser = components['schemas']['AuthUser']
type SystemInfo = components['schemas']['SystemInfo']
type KeyPair = components['schemas']['KeyPair']

/**
 * MSW handlers for the auth and system endpoints (03-frontend.md §8). The auth
 * flow cannot be tested against a real kumod, because the interesting cases are
 * the ones a healthy server refuses to produce: a rotated access token, a
 * refresh family that was revoked, a setup that is already closed.
 *
 * MSW keeps the cookie jar itself: the `Set-Cookie` on a login or refresh is
 * stored and sent back on the next request to the same origin, exactly as the
 * browser would with the HttpOnly `kumo_refresh` cookie. `resetMockState()`
 * clears the issued tokens, which is the equivalent of a fresh browser.
 */
const REFRESH_COOKIE = 'kumo_refresh'

const users: AuthUser[] = [
  {
    id: '01K0USER0000000000000001',
    username: 'ada',
    role: 'admin',
    created_at: '2026-01-02T10:00:00Z',
  },
  {
    id: '01K0USER0000000000000002',
    username: 'eve',
    role: 'viewer',
    created_at: '2026-01-02T10:05:00Z',
  },
]

const passwords = new Map<string, string>([
  ['ada', 'correct horse battery staple'],
  ['eve', 'a long enough passphrase'],
])

/** Live sessions, by token. A refresh token belongs to one user at a time. */
let refreshTokens = new Map<string, AuthUser>()
let accessTokens = new Map<string, AuthUser>()
let counter = 0
let info: SystemInfo = {
  version: 'test',
  needs_setup: false,
  hypervisor: { type: 'fake', ok: true },
}

export const MOCK_ADMIN = users[0] as AuthUser
export const MOCK_VIEWER = users[1] as AuthUser
export const MOCK_ADMIN_PASSWORD = passwords.get('ada') as string

/**
 * Key pair fixtures. The fingerprints are the OpenSSH ones of these exact public
 * keys, the same two the Go service tests check against `ssh-keygen -lf`, so a
 * console screenshot and a backend test describe the same key material.
 */
const MOCK_ED25519_PUBLIC_KEY =
  'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMSkpB4KwlcQbdHTekHMstz+XeDU0Eugj1nOCRMlwz0e fixture-ed25519'
const MOCK_RSA_PUBLIC_KEY =
  'ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAACAQDMmL6FNAluCYZGNQs4/76CBVsslCSRwqH8pt6zpeUVKrxAo90ZF/4TPpTNhgkCEyjAnONZGSA6Fnkm3EqCRyKYe6QkW/Yhra7SQXuGjXhceAu2kaMDrWpWGt3xVLCNtLyLGhLz4/yWQdkdg3i5AFoO2EEuJXp88MWUSaNcRnEOvQAEbLGbU9mw9ICDH2fkWSI5Jbbaur7+a1W6C0NO+O/QIRi7y4Li79XStDHRbaJmxu0L771QVDOU/Uyl+Dy7pzDTrs6E0SoZulVBSSzhlQEiZb2F7MDm7VFBfBaVUtLxDF5k7A2TKair0EaR4i0fbje5G7sDWWuBXv3GwmxVXvImtYb9gyFVKzRcw4A3+q+3zZuaXGhd87Zit33G0A4vMbH/mntQ195C7twWUsHIWV4L22dngBbXj8W2uMnrn/x0vrFLWyBYDiIntq1i+DbJdB9NVIXPUNH01og8hIQJLvoz+JYM894gGwebrD676zxNHvnn8PV5IsG9ccTQNTgmVS3h9ndN/Ozaisr0OYlpJh+sL+5OGzL7MnX6DRLIGTbfDy7yBGLv1mCh8Bl/z+jTDgBhB5vJAiUofGVi2taAAgnBXWyRRg7s9PoBIEeV3Lym45ofYf3vyIDCoeFY2b6/NGpkUT+TKVYWXpwdtgevHxUDn1AWfOo7UpXkBxyG4Elw9w== fixture-rsa4096'

/**
 * MOCK_KEY_PAIRS is the table the key pair pages see. `used_by` is the detail
 * field that makes a delete answer 409, so one fixture is deliberately attached
 * to a running instance.
 */
export interface MockKeyPair extends KeyPair {
  used_by: string[]
}

const INITIAL_KEY_PAIRS: MockKeyPair[] = [
  {
    id: 'key-0a1b2c3d4e5f60718',
    name: 'deploy-bot',
    type: 'ed25519',
    fingerprint: 'SHA256:nE1g6wUlVBQKTyKOSJF1xO4wZDGZSJaEgxVU2je12jg',
    public_key: MOCK_ED25519_PUBLIC_KEY,
    created_at: '2026-01-02T09:30:00Z',
    tags: { team: 'platform' },
    used_by: [],
  },
  {
    id: 'key-0b2c3d4e5f6071890',
    name: 'legacy-rsa',
    type: 'rsa',
    fingerprint: 'SHA256:0X8NCHcXhs6JyZafbPwK/WtThXvo0l2qoNQTehrRGTA',
    public_key: MOCK_RSA_PUBLIC_KEY,
    created_at: '2026-01-01T16:05:00Z',
    tags: {},
    // Attached to a running instance, so deleting it without ?force is a 409.
    used_by: ['i-0a1b2c3d4e5f60718'],
  },
]

/** freshKeyPairs is a private copy of the fixtures, so no test can mutate the originals. */
function freshKeyPairs(): MockKeyPair[] {
  return INITIAL_KEY_PAIRS.map((pair) => ({
    ...pair,
    used_by: [...pair.used_by],
    tags: { ...pair.tags },
  }))
}

// Seeded at load so the very first test sees the fixtures even before
// `resetMockState` has run for it.
let keyPairs: MockKeyPair[] = freshKeyPairs()

/** MOCK_PRIVATE_KEY is the PEM the create endpoint hands out, once. */
export const MOCK_PRIVATE_KEY =
  '-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n-----END OPENSSH PRIVATE KEY-----\n'

/** mockKeyPairs is the current fixture table, for assertions. */
export function mockKeyPairs(): MockKeyPair[] {
  return keyPairs
}

/** setMockKeyPairs replaces the fixture table, e.g. to start from an empty list. */
export function setMockKeyPairs(next: MockKeyPair[]): void {
  keyPairs = next
}

/** addMockUsedBy marks a key as attached to a running instance. */
export function addMockUsedBy(id: string, instanceID: string): void {
  keyPairs = keyPairs.map((pair) =>
    pair.id === id ? { ...pair, used_by: [...pair.used_by, instanceID] } : pair,
  )
}

/**
 * resetMockState forgets every issued token and puts the instance back to
 * "setup is closed", which is the state each test starts from.
 */
/**
 * resetMockState forgets every issued token and puts the instance back to
 * "setup is closed", which is the state each test starts from.
 *
 * `counter` is deliberately NOT reset. MSW keeps the cookie jar between tests,
 * and a cookie from an earlier test would authenticate itself against a
 * same-numbered token issued by a later one — sessions leaking across tests, at
 * random. Token ids therefore stay unique for the life of the process.
 */
export function resetMockState(): void {
  refreshTokens = new Map()
  accessTokens = new Map()
  info = { ...info, needs_setup: false }
  keyPairs = freshKeyPairs()
}

/** setMockNeedsSetup opens or closes the first-run setup path. */
export function setMockNeedsSetup(needsSetup: boolean): void {
  info = { ...info, needs_setup: needsSetup }
}

/** addMockUser registers a user a test can sign in as. */
export function addMockUser(user: AuthUser, password: string): void {
  users.push(user)
  passwords.set(user.username, password)
}

function apiError(status: number, code: components['schemas']['ErrorCode'], message: string) {
  return HttpResponse.json({ error: { code, message, request_id: 'msw' } }, { status })
}

function unauthenticated() {
  return apiError(401, 'unauthenticated', 'the session is missing or has expired')
}

function bearer(request: Request): string | null {
  const header = request.headers.get('authorization')
  return header?.startsWith('Bearer ') ? header.slice('Bearer '.length) : null
}

/** issueTokens starts a session, returning the access token and the cookie. */
function issueTokens(user: AuthUser) {
  const refreshToken = `refresh-${(counter += 1)}`
  refreshTokens.set(refreshToken, user)
  const accessToken = `access-${refreshToken}`
  accessTokens.set(accessToken, user)
  return {
    access_token: accessToken,
    expires_in: 900,
    cookie: `${REFRESH_COOKIE}=${refreshToken}; Path=/api/v1/auth; HttpOnly; SameSite=Strict`,
  }
}

export const handlers = [
  http.post('/api/v1/auth/login', async ({ request }) => {
    const body = (await request.json()) as components['schemas']['AuthLoginRequest']
    const user = users.find((candidate) => candidate.username === body.username)
    if (user === undefined || passwords.get(user.username) !== body.password) {
      // The exact message the backend answers, so the console shows the same
      // text the operator would see in production.
      return apiError(401, 'unauthenticated', 'the username or password is wrong')
    }
    const { cookie, ...tokens } = issueTokens(user)
    return HttpResponse.json({ ...tokens, user }, { headers: { 'Set-Cookie': cookie } })
  }),

  http.post('/api/v1/auth/setup', async ({ request }) => {
    const body = (await request.json()) as components['schemas']['AuthSetupRequest']
    if (!info.needs_setup) {
      return apiError(409, 'conflict', 'the first administrator already exists')
    }
    const user: AuthUser = {
      id: `01K0USER000000000000000${users.length + 1}`,
      username: body.username,
      role: 'admin',
      created_at: '2026-01-02T10:00:00Z',
    }
    addMockUser(user, body.password)
    setMockNeedsSetup(false)
    return HttpResponse.json(user, { status: 201 })
  }),

  // Rotation: the presented token is dropped, so a replay of it is refused the
  // way the backend refuses one.
  http.post('/api/v1/auth/refresh', ({ request }) => {
    const presented = readRefreshCookie(request)
    if (presented === undefined) {
      return unauthenticated()
    }
    const user = refreshTokens.get(presented)
    if (user === undefined) {
      return unauthenticated()
    }
    refreshTokens.delete(presented)
    const { cookie, ...tokens } = issueTokens(user)
    return HttpResponse.json(tokens, { headers: { 'Set-Cookie': cookie } })
  }),

  http.post('/api/v1/auth/logout', ({ request }) => {
    const presented = readRefreshCookie(request)
    if (presented === undefined) {
      return unauthenticated()
    }
    refreshTokens.delete(presented)
    accessTokens = new Map()
    return HttpResponse.json({}, { status: 204, headers: { 'Set-Cookie': clearCookie() } })
  }),

  http.get('/api/v1/auth/me', ({ request }) => {
    const token = bearer(request)
    const user = token === null ? undefined : accessTokens.get(token)
    return user === undefined ? unauthenticated() : HttpResponse.json(user)
  }),

  http.get('/api/v1/system/info', () => HttpResponse.json(info)),

  http.get('/api/v1/key-pairs', ({ request }) => {
    const user = requireUser(request)
    if (user === null) {
      return unauthenticated()
    }
    const query = new URL(request.url).searchParams.get('q')
    const items = keyPairs
      .filter((pair) => query === null || pair.name.toLowerCase().includes(query.toLowerCase()))
      // Newest first, the API's default order.
      .sort((a, b) => b.created_at.localeCompare(a.created_at))
      .map(({ used_by: _usedBy, ...pair }) => pair)
    return HttpResponse.json({ items, next_page_token: null })
  }),

  http.post('/api/v1/key-pairs', async ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const body = (await request.json()) as components['schemas']['CreateKeyPairRequest']
    const fieldError = validateNewKeyPair(body.name, keyPairs)
    if (fieldError !== null) {
      return invalidArgument(fieldError.field, fieldError.reason)
    }
    const generated = generateMockKey(body.name, body.type ?? 'ed25519', body.tags ?? {})
    keyPairs = [generated, ...keyPairs]
    // The private key rides along here and is never stored: closing the dialog
    // destroys it, exactly as the backend behaves.
    return HttpResponse.json(
      { ...stripInternal(generated), private_key: MOCK_PRIVATE_KEY },
      { status: 201 },
    )
  }),

  http.post('/api/v1/key-pairs/import', async ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const body = (await request.json()) as components['schemas']['ImportKeyPairRequest']
    const fieldError = validateNewKeyPair(body.name, keyPairs)
    if (fieldError !== null) {
      return invalidArgument(fieldError.field, fieldError.reason)
    }
    const publicKey = body.public_key.trim()
    if (publicKey === '') {
      return invalidArgument('public_key', 'must not be empty')
    }
    if (publicKey.includes('PRIVATE KEY-----')) {
      return invalidArgument('public_key', 'private key material must not be imported')
    }
    if (!publicKey.startsWith('ssh-ed25519 ') && !publicKey.startsWith('ssh-rsa ')) {
      return invalidArgument('public_key', 'is not an OpenSSH public key')
    }
    const imported: MockKeyPair = {
      ...generateMockKey(
        body.name,
        publicKey.startsWith('ssh-rsa ') ? 'rsa' : 'ed25519',
        body.tags ?? {},
      ),
      public_key: publicKey,
    }
    keyPairs = [imported, ...keyPairs]
    return HttpResponse.json(stripInternal(imported), { status: 201 })
  }),

  http.delete('/api/v1/key-pairs/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const id = String(params.id)
    const pair = keyPairs.find((candidate) => candidate.id === id)
    if (pair === undefined) {
      return apiError(404, 'not_found', `no key pair "${id}"`)
    }
    const force = new URL(request.url).searchParams.get('force') === 'true'
    if (!force && pair.used_by.length > 0) {
      return apiError(
        409,
        'conflict',
        `the key pair "${pair.name}" is in use by ${pair.used_by.join(', ')}`,
      )
    }
    keyPairs = keyPairs.filter((candidate) => candidate.id !== id)
    return new HttpResponse(null, { status: 204 })
  }),
]

/**
 * requireUser is the authenticated caller behind a request, or undefined when
 * the access token is missing or stale. It mirrors what the backend middleware
 * does, so a test cannot pass by skipping the session.
 */
function requireUser(request: Request): AuthUser | undefined {
  const token = bearer(request)
  return token === null ? undefined : accessTokens.get(token)
}

function forbidden() {
  return apiError(403, 'permission_denied', 'this action requires the admin role')
}

function invalidArgument(field: string, reason: string) {
  return HttpResponse.json(
    {
      error: {
        code: 'invalid_argument',
        message: 'the request is not acceptable',
        request_id: 'msw',
        details: [{ field, reason }],
      },
    },
    { status: 400 },
  )
}

/** validateNewKeyPair is the shared name rule of the create and import forms. */
function validateNewKeyPair(
  name: string,
  existing: MockKeyPair[],
): { field: string; reason: string } | null {
  if (!/^[A-Za-z0-9._-]{1,64}$/.test(name)) {
    return {
      field: 'name',
      reason: "must be 1-64 characters of letters, digits, '.', '_' or '-'",
    }
  }
  if (existing.some((pair) => pair.name === name)) {
    return { field: 'name', reason: `a key pair named "${name}" already exists` }
  }
  return null
}

/** generateMockKey mints a fixture row with a unique id and the current clock. */
function generateMockKey(
  name: string,
  type: 'ed25519' | 'rsa',
  tags: Record<string, string>,
): MockKeyPair {
  const suffix = String(counter + 1).padStart(3, '0')
  return {
    id: `key-0f1e2d3c4b5a6${suffix}`,
    name,
    type,
    // Placeholder digests: the console shows the fingerprint but never derives
    // it, and a fixture that looks like a real one invites wrong assertions.
    fingerprint:
      type === 'rsa'
        ? 'SHA256:Qm9ndXNSQTNwUExWW0dXQ1VpZ3RoYVNDSlRPWW5tZUkxTUkQ9PQ'
        : 'SHA256:Q2xpY2tzbW9ja0V4ZWRnZWQyNTUxOUp5U1dVSVhKQVFHVkFCQQ==',
    public_key: type === 'rsa' ? MOCK_RSA_PUBLIC_KEY : MOCK_ED25519_PUBLIC_KEY,
    created_at: '2026-01-02T10:00:00Z',
    tags,
    used_by: [],
  }
}

/** stripInternal drops `used_by`, which only the detail response carries. */
function stripInternal({ used_by: _usedBy, ...pair }: MockKeyPair): KeyPair {
  return pair
}

/** readRefreshCookie is the HttpOnly cookie the browser would send back. */
function readRefreshCookie(request: Request): string | undefined {
  const header = request.headers.get('cookie')
  if (header === null) {
    return undefined
  }
  for (const pair of header.split(';')) {
    const [name, ...value] = pair.trim().split('=')
    if (name === REFRESH_COOKIE) {
      return value.join('=')
    }
  }
  return undefined
}

/** clearCookie expires the refresh cookie the way a sign-out must. */
function clearCookie(): string {
  return `${REFRESH_COOKIE}=; Path=/api/v1/auth; HttpOnly; SameSite=Strict; Max-Age=0`
}
