import { http, HttpResponse } from 'msw'

import type { components } from '@/types/api'

type AuthUser = components['schemas']['AuthUser']
type SystemInfo = components['schemas']['SystemInfo']

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
 * resetMockState forgets every issued token and puts the instance back to
 * "setup is closed", which is the state each test starts from.
 */
export function resetMockState(): void {
  refreshTokens = new Map()
  accessTokens = new Map()
  counter = 0
  info = { ...info, needs_setup: false }
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
]

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
