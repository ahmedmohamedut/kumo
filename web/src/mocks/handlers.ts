import { http, HttpResponse } from 'msw'

import type { components } from '@/types/api'

type AuthUser = components['schemas']['AuthUser']
type SystemInfo = components['schemas']['SystemInfo']
type KeyPair = components['schemas']['KeyPair']
type Image = components['schemas']['Image']
type Task = components['schemas']['Task']
type CatalogImage = components['schemas']['CatalogImage']

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
 * Image and task fixtures.
 *
 * The four image states are all represented, because the console has to render
 * each one and the interesting rendering (a progress bar, a failure reason) only
 * exists for some of them. `referenced_by` is the same internal field the key
 * pair fixtures carry as `used_by`: it is stripped from every response and is
 * what makes a delete answer 409.
 */
export interface MockImage extends Image {
  referenced_by: string[]
}

const INITIAL_IMAGES: MockImage[] = [
  {
    id: 'img-0a1b2c3d4e5f60718',
    name: 'ubuntu-22.04',
    description: 'Ubuntu 22.04 LTS, cloud image',
    state: 'available',
    state_reason: null,
    progress: 100,
    format: 'qcow2',
    architecture: 'x86_64',
    os_family: 'Ubuntu',
    os_version: '22.04',
    default_user: 'ubuntu',
    source_url: 'https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img',
    checksum: null,
    size_bytes: 687_997_184,
    min_disk_gb: 8,
    task_id: 'task-0a1b2c3d4e5f60701',
    created_at: '2026-01-02T09:30:00Z',
    updated_at: '2026-01-02T09:41:00Z',
    tags: { base: 'cloud' },
    referenced_by: [],
  },
  {
    id: 'img-0b2c3d4e5f6071890',
    name: 'debian-12-generic',
    state: 'downloading',
    state_reason: null,
    progress: 42,
    architecture: 'x86_64',
    os_family: 'Debian',
    os_version: '12',
    source_url:
      'https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2',
    size_bytes: null,
    min_disk_gb: 8,
    task_id: 'task-0a1b2c3d4e5f60702',
    created_at: '2026-01-03T08:00:00Z',
    updated_at: '2026-01-03T08:02:00Z',
    tags: {},
    referenced_by: [],
  },
  {
    id: 'img-0c2d3e4f5a6071819',
    name: 'rocky-9-arm64',
    state: 'failed',
    state_reason: 'the source returned 403 for the requested image',
    progress: 0,
    architecture: 'aarch64',
    os_family: 'Rocky',
    os_version: '9',
    source_url:
      'https://dl.rockylinux.org/vault/rocky/9/images/arm64/Rocky-9-GenericCloud.latest.arm64.qcow2',
    min_disk_gb: 10,
    task_id: 'task-0a1b2c3d4e5f60703',
    created_at: '2026-01-04T12:00:00Z',
    updated_at: '2026-01-04T12:00:30Z',
    tags: {},
    referenced_by: [],
  },
  {
    id: 'img-0d3e4f5a6071829',
    name: 'alpine-3.20',
    state: 'available',
    state_reason: null,
    progress: 100,
    format: 'qcow2',
    architecture: 'x86_64',
    os_family: 'Alpine',
    os_version: '3.20',
    default_user: 'alpine',
    source_url:
      'https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/cloud/alpine-3.20.1-x86_64.qcow2',
    size_bytes: 67_108_864,
    min_disk_gb: 1,
    task_id: 'task-0a1b2c3d4e5f60704',
    created_at: '2026-01-05T07:15:00Z',
    updated_at: '2026-01-05T07:16:00Z',
    tags: {},
    // A running instance launches from it, so deleting it answers 409.
    referenced_by: ['i-0a1b2c3d4e5f60718'],
  },
]

const INITIAL_TASKS: Task[] = [
  {
    id: 'task-0a1b2c3d4e5f60701',
    type: 'image.import',
    resource_id: 'img-0a1b2c3d4e5f60718',
    state: 'succeeded',
    progress: 100,
    created_at: '2026-01-02T09:30:00Z',
    updated_at: '2026-01-02T09:41:00Z',
  },
  {
    id: 'task-0a1b2c3d4e5f60702',
    type: 'image.import',
    resource_id: 'img-0b2c3d4e5f6071890',
    state: 'running',
    progress: 42,
    created_at: '2026-01-03T08:00:00Z',
    updated_at: '2026-01-03T08:02:00Z',
  },
  {
    id: 'task-0a1b2c3d4e5f60703',
    type: 'image.import',
    resource_id: 'img-0c2d3e4f5a6071819',
    state: 'failed',
    error: 'the source returned 403 for the requested image',
    created_at: '2026-01-04T12:00:00Z',
    updated_at: '2026-01-04T12:00:30Z',
  },
  {
    id: 'task-0a1b2c3d4e5f60704',
    type: 'image.import',
    resource_id: 'img-0d3e4f5a6071829',
    state: 'succeeded',
    progress: 100,
    created_at: '2026-01-05T07:15:00Z',
    updated_at: '2026-01-05T07:16:00Z',
  },
]

/**
 * The catalog of well-known images the import page offers, the same three entries
 * the backend serves first (`internal/service/image/catalog.go`).
 *
 * The ids are bare tokens and the names are picker labels: the console turns the
 * id into the image name, because a name may not contain the spaces and
 * parentheses a label wants.
 */
const CATALOG: CatalogImage[] = [
  {
    id: 'ubuntu-2404',
    name: 'Ubuntu 24.04 LTS',
    description: "Canonical's long term support release, server image",
    os_family: 'ubuntu',
    os_version: '24.04',
    architecture: 'x86_64',
    default_user: 'ubuntu',
    min_disk_gb: 8,
    source_url:
      'https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img',
    checksum_url: 'https://cloud-images.ubuntu.com/releases/noble/release/SHA256SUMS',
  },
  {
    id: 'debian-12',
    name: 'Debian 12 (bookworm)',
    description: "Debian's current stable release, generic cloud image",
    os_family: 'debian',
    os_version: '12',
    architecture: 'x86_64',
    default_user: 'debian',
    min_disk_gb: 8,
    source_url:
      'https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2',
  },
  {
    id: 'alpine-320',
    name: 'Alpine 3.20',
    description: 'Small general purpose image',
    os_family: 'alpine',
    os_version: '3.20',
    architecture: 'x86_64',
    default_user: 'alpine',
    min_disk_gb: 1,
    source_url:
      'https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/cloud/alpine-3.20.1-x86_64.qcow2',
  },
]

/** freshImages and freshTasks are private copies, so no test can mutate the originals. */
function freshImages(): MockImage[] {
  return INITIAL_IMAGES.map((image) => ({
    ...image,
    tags: { ...image.tags },
    referenced_by: [...image.referenced_by],
  }))
}

function freshTasks(): Task[] {
  return INITIAL_TASKS.map((task) => ({ ...task }))
}

let images: MockImage[] = freshImages()
let tasks: Task[] = freshTasks()

/** mockImages is the current fixture table, for assertions. */
export function mockImages(): MockImage[] {
  return images
}

/** mockTasks is the current task table, for assertions. */
export function mockTasks(): Task[] {
  return tasks
}

/** setMockImages replaces the fixture table, e.g. to start from an empty list. */
export function setMockImages(next: MockImage[]): void {
  images = next
}

/** addMockImageReference marks an image as launched-from by an instance. */
export function addMockImageReference(id: string, instanceID: string): void {
  images = images.map((image) =>
    image.id === id ? { ...image, referenced_by: [...image.referenced_by, instanceID] } : image,
  )
}

/**
 * nextResourceID mints an id like the ones kumod generates (`img-`/`task-` plus
 * 17 hex characters), derived from the shared counter so two fixtures minted in
 * the same test cannot collide.
 */
function nextResourceID(prefix: 'img' | 'task'): string {
  return `${prefix}-0f1e2d3c4b5a6${String((counter += 1)).padStart(5, '0')}`
}

/** stripImage drops the internal `referenced_by`, which no response carries. */
function stripImage({ referenced_by: _referencedBy, ...image }: MockImage): Image {
  return image
}

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
  images = freshImages()
  tasks = freshTasks()
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

  // ---------------------------------------------------------------- images ---

  http.get('/api/v1/images', ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    const params = new URL(request.url).searchParams
    const query = params.get('q')
    const state = params.get('state')
    const items = images
      // `q` matches the name only, the same as the SQL the backend runs.
      .filter((image) => query === null || image.name.toLowerCase().includes(query.toLowerCase()))
      .filter((image) => state === null || image.state === state)
      // Newest first, the API's default order.
      .sort((a, b) => b.created_at.localeCompare(a.created_at))
      .map(stripImage)
    return HttpResponse.json({ items, next_page_token: null })
  }),

  http.get('/api/v1/images/catalog', ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    return HttpResponse.json({ items: CATALOG, next_page_token: null })
  }),

  // The import answers 202 with the image in `pending` and the id of the task
  // that will do the work. It never waits for the download, which is the whole
  // point of the task (01-api.md §Images).
  http.post('/api/v1/images/import', async ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const body = (await request.json()) as components['schemas']['ImportImageRequest']
    const fieldError = validateNewImage(body.name, body.source_url, body.os_family, images)
    if (fieldError !== null) {
      return invalidArgument(fieldError.field, fieldError.reason)
    }
    const task: Task = {
      id: nextResourceID('task'),
      type: 'image.import',
      state: 'pending',
      created_at: '2026-01-06T00:00:00Z',
      updated_at: '2026-01-06T00:00:00Z',
    }
    const image: MockImage = {
      id: nextResourceID('img'),
      name: body.name,
      description: body.description,
      state: 'pending',
      state_reason: null,
      progress: 0,
      architecture: body.architecture,
      os_family: body.os_family,
      os_version: body.os_version,
      default_user: body.default_user,
      source_url: body.source_url,
      checksum: body.checksum ?? null,
      min_disk_gb: body.min_disk_gb,
      task_id: task.id,
      created_at: '2026-01-06T00:00:00Z',
      updated_at: '2026-01-06T00:00:00Z',
      tags: { ...body.tags },
      referenced_by: [],
    }
    tasks = [task, ...tasks]
    images = [image, ...images]
    return HttpResponse.json(stripImage(image), { status: 202 })
  }),

  http.get('/api/v1/images/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    const image = images.find((candidate) => candidate.id === params.id)
    if (image === undefined) {
      return apiError(404, 'not_found', `no image "${String(params.id)}"`)
    }
    return HttpResponse.json(stripImage(image))
  }),

  http.patch('/api/v1/images/:id', async ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const body = (await request.json()) as components['schemas']['UpdateImageRequest']
    const id = String(params.id)
    const image = images.find((candidate) => candidate.id === id)
    if (image === undefined) {
      return apiError(404, 'not_found', `no image "${id}"`)
    }
    if (body.name !== undefined && body.name !== image.name) {
      const fieldError = validateNewImage(body.name, image.source_url ?? '', 'linux', images)
      if (fieldError !== null) {
        return invalidArgument(fieldError.field, fieldError.reason)
      }
    }
    const updated: MockImage = {
      ...image,
      ...(body.name === undefined ? {} : { name: body.name }),
      ...(body.description === undefined ? {} : { description: body.description }),
      ...(body.tags === undefined ? {} : { tags: { ...body.tags } }),
      updated_at: '2026-01-06T01:00:00Z',
    }
    images = images.map((candidate) => (candidate.id === id ? updated : candidate))
    return HttpResponse.json(stripImage(updated))
  }),

  // Deleting is a task too: the answer is 202 with the image in `deleting`, and
  // the row only disappears when that task finishes. The mock leaves it there so
  // a test can watch the state the console renders.
  http.delete('/api/v1/images/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const id = String(params.id)
    const image = images.find((candidate) => candidate.id === id)
    if (image === undefined) {
      return apiError(404, 'not_found', `no image "${id}"`)
    }
    if (image.referenced_by.length > 0) {
      return apiError(
        409,
        'conflict',
        `the image "${image.name}" is still referenced by ${image.referenced_by.join(', ')}`,
      )
    }
    const task: Task = {
      id: nextResourceID('task'),
      type: 'image.delete',
      resource_id: id,
      state: 'pending',
      created_at: '2026-01-06T02:00:00Z',
      updated_at: '2026-01-06T02:00:00Z',
    }
    tasks = [task, ...tasks]
    images = images.map((candidate) =>
      candidate.id === id
        ? {
            ...candidate,
            state: 'deleting' as const,
            task_id: task.id,
            updated_at: task.created_at,
          }
        : candidate,
    )
    return HttpResponse.json(
      stripImage(images.find((candidate) => candidate.id === id) as MockImage),
      {
        status: 202,
      },
    )
  }),

  http.post('/api/v1/images/:id/retry', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const id = String(params.id)
    const image = images.find((candidate) => candidate.id === id)
    if (image === undefined) {
      return apiError(404, 'not_found', `no image "${id}"`)
    }
    if (image.state !== 'failed') {
      return apiError(409, 'conflict', `the image "${image.name}" is not in a failed state`)
    }
    const task: Task = {
      id: nextResourceID('task'),
      type: 'image.import',
      resource_id: id,
      state: 'pending',
      created_at: '2026-01-06T03:00:00Z',
      updated_at: '2026-01-06T03:00:00Z',
    }
    tasks = [task, ...tasks]
    images = images.map((candidate) =>
      candidate.id === id
        ? {
            ...candidate,
            state: 'pending' as const,
            state_reason: null,
            progress: 0,
            task_id: task.id,
            updated_at: task.created_at,
          }
        : candidate,
    )
    return HttpResponse.json(
      stripImage(images.find((candidate) => candidate.id === id) as MockImage),
      { status: 202 },
    )
  }),

  // ----------------------------------------------------------------- tasks ---

  http.get('/api/v1/tasks', ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    const params = new URL(request.url).searchParams
    const resourceId = params.get('resource_id')
    const state = params.get('state')
    const items = tasks
      .filter((task) => resourceId === null || task.resource_id === resourceId)
      .filter((task) => state === null || task.state === state)
      .sort((a, b) => b.created_at.localeCompare(a.created_at))
    return HttpResponse.json({ items, next_page_token: null })
  }),

  http.get('/api/v1/tasks/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    const task = tasks.find((candidate) => candidate.id === params.id)
    if (task === undefined) {
      return apiError(404, 'not_found', `no task "${String(params.id)}"`)
    }
    return HttpResponse.json(task)
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

/** validateNewImage is the shared import rule: a name, an http(s) URL and an OS. */
function validateNewImage(
  name: string,
  sourceURL: string,
  osFamily: string,
  existing: MockImage[],
): { field: string; reason: string } | null {
  if (!/^[A-Za-z0-9._-]{1,128}$/.test(name)) {
    return {
      field: 'name',
      reason: "must be 1-128 characters of letters, digits, '.', '_' or '-'",
    }
  }
  if (existing.some((image) => image.name === name)) {
    return { field: 'name', reason: `an image named "${name}" already exists` }
  }
  if (!/^https?:\/\//i.test(sourceURL)) {
    return { field: 'source_url', reason: 'must be an http or https URL' }
  }
  if (osFamily.trim() === '') {
    return { field: 'os_family', reason: 'must not be empty' }
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
