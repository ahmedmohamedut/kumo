import { http, HttpResponse } from 'msw'

import type { components } from '@/types/api'

type AuthUser = components['schemas']['AuthUser']
type SystemInfo = components['schemas']['SystemInfo']
type KeyPair = components['schemas']['KeyPair']
type Image = components['schemas']['Image']
type Task = components['schemas']['Task']
type CatalogImage = components['schemas']['CatalogImage']
type Instance = components['schemas']['Instance']
type InstanceType = components['schemas']['InstanceType']
type LaunchInstanceRequest = components['schemas']['LaunchInstanceRequest']
type VPC = components['schemas']['VPC']
type Subnet = components['schemas']['Subnet']
type Bucket = components['schemas']['Bucket']
type BucketDetail = components['schemas']['BucketDetail']
type AccessKey = components['schemas']['AccessKey']
type DBInstance = components['schemas']['DBInstance']
type Cluster = components['schemas']['Cluster']
type SparkCluster = components['schemas']['SparkCluster']

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
  object_storage: { backend: 'fake', ok: true },
  databases: { enabled: true, engines: [{ engine: 'postgres', version: '18' }] },
  kubernetes: { enabled: true, versions: ['1.37'] },
  spark: { enabled: true, versions: ['4.3'], object_storage: true },
}

/** setMockObjectStorage switches the object storage module, e.g. to off. */
export function setMockObjectStorage(objectStorage: SystemInfo['object_storage']): void {
  info = { ...info, object_storage: objectStorage }
}

/**
 * setMockDatabases switches the databases module: off, or on with no engine
 * image, which is the normal state of a node where nobody has imported one
 * (07-databases.md section 2).
 */
export function setMockDatabases(databases: SystemInfo['databases']): void {
  info = { ...info, databases }
}

/** MOCK_ENGINES is the engine list a node with the Postgres image reports. */
export const MOCK_ENGINES: SystemInfo['databases']['engines'] = [
  { engine: 'postgres', version: '18' },
]

/**
 * MockBucket is a bucket fixture plus what the server knows about it and the
 * list endpoint does not return: its versioning state and how many objects it
 * holds, which decides whether a plain delete is refused.
 */
export interface MockBucket {
  bucket: Bucket
  versioning: BucketDetail['versioning']
  objects: MockObject[]
}

/** MockObject is one key in a mock bucket; a key ending in '/' is a folder marker. */
export interface MockObject {
  key: string
  size_bytes: number
  last_modified: string
  content_type?: string
}

function mockObject(key: string, size = 1024): MockObject {
  return { key, size_bytes: size, last_modified: '2026-01-05T11:00:00Z' }
}

export const MOCK_S3_ENDPOINT = 'http://s3.kumo.test:9000'

/** findBucket looks a bucket up for the object handlers. */
function findBucket(name: string): MockBucket | undefined {
  return buckets.find((candidate) => candidate.bucket.name === name)
}

/** listFolder is the S3 ListObjectsV2 of the mock: one level under prefix,
 *  folders as common prefixes, paged by index. */
function listFolder(entry: MockBucket, prefix: string, cursor: string | null, limit: number) {
  const names = new Set<string>()
  const rows: Array<{ name: string; folder: boolean; object?: MockObject }> = []
  for (const object of entry.objects) {
    if (!object.key.startsWith(prefix)) {
      continue
    }
    const rest = object.key.slice(prefix.length)
    const slash = rest.indexOf('/')
    if (slash >= 0 && slash < rest.length - 1) {
      const folder = prefix + rest.slice(0, slash + 1)
      if (!names.has(folder)) {
        names.add(folder)
        rows.push({ name: folder, folder: true })
      }
      continue
    }
    if (rest.endsWith('/') && rest !== '') {
      if (!names.has(object.key)) {
        names.add(object.key)
        rows.push({ name: object.key, folder: true })
      }
      continue
    }
    if (rest === '') {
      continue // the marker of the folder being listed
    }
    rows.push({ name: object.key, folder: false, object })
  }
  rows.sort((a, b) => a.name.localeCompare(b.name))
  const start = cursor === null ? 0 : Number(cursor)
  const page = rows.slice(start, start + limit)
  const next = start + limit < rows.length ? String(start + limit) : null
  return {
    prefix,
    folders: page.filter((row) => row.folder).map((row) => row.name),
    objects: page
      .filter((row) => !row.folder && row.object !== undefined)
      .map((row) => ({
        key: row.name,
        size_bytes: row.object?.size_bytes ?? 0,
        last_modified: row.object?.last_modified ?? '',
        etag: 'mock-etag',
      })),
    next_cursor: next,
  }
}

/** encodeKey escapes each segment of a key, keeping its slashes. */
function encodeKey(key: string): string {
  return key.split('/').map(encodeURIComponent).join('/')
}

/** writableBucket is the guard the object writes share: the bucket exists and
 *  is not being deleted. */
function writableBucket(name: string) {
  const entry = findBucket(name)
  if (entry === undefined) {
    return { refusal: apiError(404, 'not_found', `no bucket "${name}"`) }
  }
  if (entry.bucket.state === 'deleting') {
    return { refusal: apiError(409, 'conflict', `bucket "${name}" is being deleted`) }
  }
  return { entry }
}

function freshBuckets(): MockBucket[] {
  return [
    {
      bucket: {
        name: 'app-logs',
        state: 'available',
        created_at: '2026-01-05T10:00:00Z',
        created_by: 'ada',
        tags: { env: 'prod' },
      },
      versioning: 'enabled',
      objects: [
        mockObject('README.md', 2048),
        mockObject('logs/'),
        mockObject('logs/2026-01-04.log', 524288),
        mockObject('logs/2026-01-05.log', 1048576),
        mockObject('logs/archive/2025.tar.gz', 73400320),
      ],
    },
    {
      bucket: {
        name: 'backups',
        state: 'available',
        created_at: '2026-01-04T08:30:00Z',
        created_by: null,
        tags: {},
      },
      versioning: 'off',
      objects: [],
    },
  ]
}

let buckets: MockBucket[] = freshBuckets()

function freshAccessKeys(): AccessKey[] {
  return [
    {
      access_key: 'KUMOAAAABBBBCCCCDDDD',
      name: 'log-shipper',
      level: 'read-write',
      buckets: ['app-logs'],
      state: 'active',
      created_by: 'ada',
      created_at: '2026-01-05T12:00:00Z',
      updated_at: '2026-01-05T12:00:00Z',
    },
  ]
}

let accessKeys: AccessKey[] = freshAccessKeys()

/** mockAccessKeys is the current access key table, for assertions. */
export function mockAccessKeys(): AccessKey[] {
  return accessKeys
}

/** setMockAccessKeys replaces the access key table. */
export function setMockAccessKeys(next: AccessKey[]): void {
  accessKeys = next
}

/** listPageSize is the default page of the mock listing; tests lower it to page. */
let listPageSize = 200

/** setMockObjectPageSize sets the default listing page, to exercise paging. */
export function setMockObjectPageSize(size: number): void {
  listPageSize = size
}

/** mockBuckets is the current bucket table, for assertions. */
export function mockBuckets(): MockBucket[] {
  return buckets
}

/** setMockBuckets replaces the bucket table, e.g. to start from an empty list. */
export function setMockBuckets(next: MockBucket[]): void {
  buckets = next
}

function bucketDetail(entry: MockBucket): BucketDetail {
  return {
    ...entry.bucket,
    versioning: entry.versioning,
    endpoint: MOCK_S3_ENDPOINT,
    region: 'us-east-1',
  }
}

/** validateAccessKey applies the rules of internal/service/accesskey. */
function validateAccessKey(
  body: { name?: string; level?: string; buckets?: string[] },
  self: string | null,
) {
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/.test(body.name ?? '')) {
    return invalidArgument('name', 'must be 1-64 letters, digits, ".", "_" or "-"')
  }
  if (body.level !== 'read-only' && body.level !== 'read-write') {
    return invalidArgument('level', 'must be read-only or read-write')
  }
  const missing = (body.buckets ?? []).find((name) => findBucket(name) === undefined)
  if ((body.buckets ?? []).length === 0 || missing !== undefined) {
    return invalidArgument(
      'buckets',
      missing === undefined ? 'must name 1-100 buckets' : `no bucket "${missing}"`,
    )
  }
  if (accessKeys.some((key) => key.name === body.name && key.access_key !== self)) {
    return apiError(
      409,
      'already_exists',
      `already exists: an access key named "${body.name ?? ''}" already exists`,
    )
  }
  return null
}

/** sparkRefusal is the 503 every /spark route answers with the module off. */
function sparkRefusal() {
  if (!info.spark.enabled) {
    return apiError(
      503,
      'unavailable',
      'unavailable: the Spark module is off on this node; set KUMO_SPARK=on',
    )
  }
  return null
}

/** kubernetesRefusal is the 503 every /eks route answers with the module off. */
function kubernetesRefusal() {
  if (!info.kubernetes.enabled) {
    return apiError(
      503,
      'unavailable',
      'unavailable: the Kubernetes module is off on this node; set KUMO_KUBERNETES=on',
    )
  }
  return null
}

/** databasesRefusal is the 503 every /rds route answers with the module off. */
function databasesRefusal() {
  if (!info.databases.enabled) {
    return apiError(
      503,
      'unavailable',
      'unavailable: the databases module is off on this node; set KUMO_DATABASES=on',
    )
  }
  return null
}

/** objectStorageRefusal is the 503 every /s3 route answers with the module off or down. */
function objectStorageRefusal() {
  if (info.object_storage.backend === 'none') {
    return apiError(
      503,
      'unavailable',
      'unavailable: object storage is not configured on this node',
    )
  }
  if (!info.object_storage.ok) {
    return apiError(503, 'unavailable', 'unavailable: the object store: connection refused')
  }
  return null
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
    kind: 'user',
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
    kind: 'user',
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
    kind: 'user',
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
    kind: 'user',
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

/**
 * INITIAL_INSTANCE_TYPES are the types `GET /instance-types` starts from. They
 * are deliberately fewer than the real seed set of migration 0003 and carry
 * short descriptions: the launch form tests pick `kumo.small` by name and do
 * not care about the rest. The instance fixtures below reference `kumo.small`
 * and `kumo.medium`, which is what the delete guard of the instance types page
 * is tested against; `kumo.large` is unused.
 */
const INITIAL_INSTANCE_TYPES: InstanceType[] = [
  { id: 'kumo.small', vcpus: 2, memory_mib: 4096, description: 'Small' },
  { id: 'kumo.medium', vcpus: 4, memory_mib: 8192, description: 'Medium' },
  { id: 'kumo.large', vcpus: 8, memory_mib: 16384, description: 'Large' },
]

/**
 * Instance fixtures.
 *
 * One row per lifecycle state the list has to render, because the state cell is
 * where most of the interesting rendering lives: `web-1` is running, `web-2` is
 * still `pending` (the launch task has not finished), `db-1` carries a
 * `state_reason` so the error cell has something to show, and `old-1` is
 * `terminated` so the row that outlives its instance is covered too.
 *
 * `i-0a1b2c3d4e5f60718` is the instance the key pair and image fixtures already
 * reference, so the 409s those pages test stay consistent with these.
 */
const INITIAL_INSTANCES: Instance[] = [
  {
    id: 'i-0a1b2c3d4e5f60718',
    name: 'web-1',
    state: 'running',
    state_reason: null,
    image_id: 'img-0d3e4f5a6071829',
    image_name: 'alpine-3.20',
    instance_type: 'kumo.small',
    vcpus: 2,
    memory_mib: 4096,
    root_disk_gb: 8,
    key_pair_id: 'key-0a1b2c3d4e5f60718',
    key_pair_name: 'legacy-rsa',
    private_ip: '10.0.0.11',
    mac_address: '52:54:00:11:22:33',
    vpc_id: 'vpc-0a1b2c3d4e5f60718',
    subnet_id: 'subnet-0a1b2c3d4e5f60718',
    network_interfaces: [
      {
        id: 'eni-0a1b2c3d4e5f60718',
        device_index: 0,
        mac_address: '52:54:00:11:22:33',
        private_ip: '10.0.0.11',
        subnet_id: 'subnet-0a1b2c3d4e5f60718',
        vpc_id: 'vpc-0a1b2c3d4e5f60718',
      },
    ],
    user_data: null,
    task_id: null,
    launched_at: '2026-01-03T09:00:00Z',
    terminated_at: null,
    updated_at: '2026-01-03T09:00:40Z',
    tags: { env: 'prod' },
  },
  {
    id: 'i-0b2c3d4e5f6071890',
    name: 'web-2',
    // Launched but not yet running: the launch task is still working, which is
    // what makes the list poll at 5 s. The address was reserved in the launch
    // transaction, so the row already carries it.
    state: 'pending',
    state_reason: null,
    image_id: 'img-0b2c3d4e5f6071829',
    image_name: 'ubuntu-22.04',
    instance_type: 'kumo.small',
    vcpus: 2,
    memory_mib: 4096,
    root_disk_gb: 20,
    key_pair_id: null,
    key_pair_name: null,
    private_ip: '10.0.0.12',
    mac_address: '52:54:00:12:34:56',
    vpc_id: 'vpc-0a1b2c3d4e5f60718',
    subnet_id: 'subnet-0a1b2c3d4e5f60718',
    network_interfaces: [
      {
        id: 'eni-0b2c3d4e5f6071890',
        device_index: 0,
        mac_address: '52:54:00:12:34:56',
        private_ip: '10.0.0.12',
        subnet_id: 'subnet-0a1b2c3d4e5f60718',
        vpc_id: 'vpc-0a1b2c3d4e5f60718',
      },
    ],
    user_data: null,
    task_id: 'task-0a1b2c3d4e5f60710',
    launched_at: '2026-01-05T11:00:00Z',
    terminated_at: null,
    updated_at: '2026-01-05T11:00:00Z',
    tags: {},
  },
  {
    id: 'i-0c2d3e4f5a6071819',
    name: 'db-1',
    state: 'error',
    // A reason that names a cause and a value, so a test can assert the cell
    // carries something more than "Error".
    state_reason: 'the guest did not boot: root disk 2 GiB is below the 8 GiB image needs',
    image_id: 'img-0b2c3d4e5f6071829',
    image_name: 'ubuntu-22.04',
    instance_type: 'kumo.medium',
    vcpus: 4,
    memory_mib: 8192,
    root_disk_gb: 2,
    key_pair_id: null,
    key_pair_name: null,
    // A failed launch keeps the address it reserved (M4b.3), so db-1 still
    // holds one from the `db` subnet.
    private_ip: '10.0.1.11',
    mac_address: '52:54:00:44:55:66',
    vpc_id: 'vpc-0a1b2c3d4e5f60718',
    subnet_id: 'subnet-0b2c3d4e5f6071890',
    network_interfaces: [
      {
        id: 'eni-0c2d3e4f5a6071819',
        device_index: 0,
        mac_address: '52:54:00:44:55:66',
        private_ip: '10.0.1.11',
        subnet_id: 'subnet-0b2c3d4e5f6071890',
        vpc_id: 'vpc-0a1b2c3d4e5f60718',
      },
    ],
    user_data: null,
    task_id: 'task-0a1b2c3d4e5f60711',
    launched_at: '2026-01-04T14:00:00Z',
    terminated_at: null,
    updated_at: '2026-01-04T14:02:00Z',
    tags: {},
  },
  {
    id: 'i-0d2e3f4a5b6071829',
    name: 'old-1',
    // Terminated: the state a row an earlier release kept as history is in. A
    // terminate deletes the row now, so the console only ever meets this state
    // on such a row — it still has to render, and the name the row holds is
    // still reusable. Terminating dropped the interface (M4b.3), so no network
    // fields survive.
    state: 'terminated',
    state_reason: null,
    image_id: 'img-0d3e4f5a6071829',
    image_name: 'alpine-3.20',
    instance_type: 'kumo.small',
    vcpus: 2,
    memory_mib: 4096,
    root_disk_gb: 8,
    key_pair_id: null,
    key_pair_name: null,
    private_ip: null,
    mac_address: null,
    vpc_id: null,
    subnet_id: null,
    network_interfaces: [],
    user_data: null,
    task_id: null,
    launched_at: '2026-01-02T08:00:00Z',
    terminated_at: '2026-01-04T10:00:00Z',
    updated_at: '2026-01-04T10:00:05Z',
    tags: {},
  },
]

/** freshInstances is a private copy of the fixtures, so no test can mutate them. */
function freshInstances(): Instance[] {
  return INITIAL_INSTANCES.map((instance) => ({
    ...instance,
    tags: { ...instance.tags },
    network_interfaces: instance.network_interfaces?.map((nic) => ({ ...nic })) ?? undefined,
  }))
}

/**
 * DB instance fixtures (07-databases.md). `orders` is the ordinary case:
 * available, with an endpoint to connect to. `reports` is `failed` with the
 * reason the reconciler writes, which is the state the detail page explains
 * and the one an operator has to decide about.
 */
const INITIAL_DB_INSTANCES: DBInstance[] = [
  {
    id: 'db-0a1b2c3d4e5f60718',
    identifier: 'orders',
    state: 'available',
    state_reason: null,
    engine: 'postgres',
    engine_version: '18',
    image_id: 'img-0a1b2c3d4e5f60718',
    instance_id: 'i-0a1b2c3d4e5f60718',
    instance_type: 'kumo.medium',
    vcpus: 2,
    memory_mib: 4096,
    storage_gb: 20,
    port: 5432,
    admin_username: 'app',
    database_name: 'orders',
    endpoint: 'orders.default',
    task_id: 'task-0a1b2c3d4e5f60718',
    created_at: '2026-01-05T10:00:00Z',
    updated_at: '2026-01-05T10:02:00Z',
  },
  {
    id: 'db-0b2c3d4e5f6071890',
    identifier: 'reports',
    state: 'failed',
    state_reason: 'engine_not_responding: nothing answers on 10.0.0.9:5432',
    engine: 'postgres',
    engine_version: '18',
    image_id: 'img-0a1b2c3d4e5f60718',
    instance_id: 'i-0b2c3d4e5f6071890',
    instance_type: 'kumo.small',
    vcpus: 1,
    memory_mib: 2048,
    storage_gb: 10,
    port: 5432,
    admin_username: 'reader',
    database_name: 'reports',
    endpoint: 'reports.default',
    task_id: null,
    created_at: '2026-01-05T09:00:00Z',
    updated_at: '2026-01-05T09:30:00Z',
  },
]

function freshDBInstances(): DBInstance[] {
  return INITIAL_DB_INSTANCES.map((dbInstance) => ({ ...dbInstance }))
}

/** A fixture node of a cluster. */
function clusterNode(
  name: string,
  role: 'control-plane' | 'worker',
  ip: string,
  ready: boolean | null,
): Cluster['nodes'][number] {
  return {
    name,
    instance_id: `i-0c1d2e3f4a5b6c${ip.slice(-3).padStart(3, '0')}`,
    role,
    node_group_id: role === 'worker' ? 'ng-0a1b2c3d4e5f60718' : null,
    state: ready === null ? 'pending' : 'running',
    private_ip: ip,
    ready,
  }
}

/**
 * Cluster fixtures: `orders` is active with a control plane and two Ready
 * workers, and `billing` is still being created.
 */
const INITIAL_CLUSTERS: Cluster[] = [
  {
    id: 'eks-0a1b2c3d4e5f60718',
    name: 'orders',
    kubernetes_version: '1.37',
    state: 'active',
    state_reason: null,
    endpoint: 'https://api.orders.default:6443',
    vpc_id: 'vpc-0a1b2c3d4e5f60718',
    subnet_id: 'subnet-0a1b2c3d4e5f60718',
    pod_cidr: '100.64.0.0/16',
    service_cidr: '100.96.0.0/16',
    control_plane_type: 'kumo.medium',
    ca_certificate: '-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n',
    node_groups: [
      {
        id: 'ng-0a1b2c3d4e5f60718',
        name: 'default',
        instance_type: 'kumo.medium',
        root_disk_gb: 8,
        desired_size: 2,
      },
    ],
    nodes: [
      clusterNode('orders-cp-1', 'control-plane', '192.168.100.20', true),
      clusterNode('orders-default-1', 'worker', '192.168.100.21', true),
      clusterNode('orders-default-2', 'worker', '192.168.100.22', true),
    ],
    created_at: '2026-01-05T09:00:00Z',
    updated_at: '2026-01-05T09:05:00Z',
  },
  {
    id: 'eks-0b1c2d3e4f5a60718',
    name: 'billing',
    kubernetes_version: '1.37',
    state: 'creating',
    state_reason: null,
    endpoint: null,
    vpc_id: 'vpc-0a1b2c3d4e5f60718',
    subnet_id: 'subnet-0a1b2c3d4e5f60718',
    pod_cidr: '100.64.0.0/16',
    service_cidr: '100.96.0.0/16',
    control_plane_type: 'kumo.medium',
    ca_certificate: '-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n',
    node_groups: [
      {
        id: 'ng-0b1c2d3e4f5a60718',
        name: 'default',
        instance_type: 'kumo.medium',
        root_disk_gb: 8,
        desired_size: 1,
      },
    ],
    nodes: [
      clusterNode('billing-cp-1', 'control-plane', '192.168.100.30', null),
      clusterNode('billing-default-1', 'worker', '192.168.100.31', null),
    ],
    created_at: '2026-01-06T09:00:00Z',
    updated_at: '2026-01-06T09:00:00Z',
  },
]

/** A fixture node of a Spark cluster. */
function sparkNode(
  name: string,
  role: 'master' | 'worker',
  ip: string,
  alive: boolean | null,
): SparkCluster['nodes'][number] {
  return {
    name,
    instance_id: `i-0d1e2f3a4b5c6d${ip.slice(-3).padStart(3, '0')}`,
    role,
    node_group_id: role === 'worker' ? 'ng-0c1d2e3f4a5b60718' : null,
    state: alive === null ? 'pending' : 'running',
    private_ip: ip,
    alive,
  }
}

/**
 * Spark cluster fixtures: `etl` is active with object storage on two buckets
 * and event logs, `adhoc` is still being created without object storage.
 */
const INITIAL_SPARK_CLUSTERS: SparkCluster[] = [
  {
    id: 'spk-0a1b2c3d4e5f60718',
    name: 'etl',
    spark_version: '4.3',
    state: 'active',
    state_reason: null,
    endpoint: 'spark://etl.default:7077',
    master_ui_url: 'http://etl.default:8080',
    history_url: 'http://etl.default:18080',
    vpc_id: 'vpc-0a1b2c3d4e5f60718',
    subnet_id: 'subnet-0a1b2c3d4e5f60718',
    master_type: 'kumo.medium',
    object_storage: true,
    buckets: ['app-logs', 'backups'],
    event_log_bucket: 'app-logs',
    node_groups: [
      {
        id: 'ng-0c1d2e3f4a5b60718',
        name: 'default',
        instance_type: 'kumo.large',
        root_disk_gb: 8,
        desired_size: 2,
      },
    ],
    nodes: [
      sparkNode('etl-master', 'master', '192.168.100.50', true),
      sparkNode('etl-default-1', 'worker', '192.168.100.51', true),
      sparkNode('etl-default-2', 'worker', '192.168.100.52', true),
    ],
    created_at: '2026-01-05T09:00:00Z',
    updated_at: '2026-01-05T09:05:00Z',
  },
  {
    id: 'spk-0b1c2d3e4f5a60718',
    name: 'adhoc',
    spark_version: '4.3',
    state: 'creating',
    state_reason: null,
    endpoint: null,
    master_ui_url: null,
    history_url: null,
    vpc_id: 'vpc-0a1b2c3d4e5f60718',
    subnet_id: 'subnet-0a1b2c3d4e5f60718',
    master_type: 'kumo.medium',
    object_storage: false,
    buckets: [],
    event_log_bucket: null,
    node_groups: [
      {
        id: 'ng-0d1e2f3a4b5c60718',
        name: 'default',
        instance_type: 'kumo.large',
        root_disk_gb: 8,
        desired_size: 1,
      },
    ],
    nodes: [
      sparkNode('adhoc-master', 'master', '192.168.100.60', null),
      sparkNode('adhoc-default-1', 'worker', '192.168.100.61', null),
    ],
    created_at: '2026-01-06T09:00:00Z',
    updated_at: '2026-01-06T09:00:00Z',
  },
]

function freshSparkClusters(): SparkCluster[] {
  return INITIAL_SPARK_CLUSTERS.map((cluster) => structuredClone(cluster))
}

function freshClusters(): Cluster[] {
  return INITIAL_CLUSTERS.map((cluster) => structuredClone(cluster))
}

/**
 * VPC and subnet fixtures. The default VPC and its default subnet are what the
 * backend bootstraps on first start, and web-1/web-2 hold addresses from the
 * default subnet while db-1 holds one from the `db` subnet — so the "in use"
 * 409s the pages guard against line up with the instance fixtures. `prod`
 * carries no instances, so its subnet can be deleted and its VPC with it.
 */
const INITIAL_VPCS: VPC[] = [
  {
    id: 'vpc-0a1b2c3d4e5f60718',
    name: 'default',
    cidr: '10.0.0.0/16',
    mode: 'nat',
    gateway_ip: '10.0.0.1',
    netmask: '255.255.0.0',
    bridge_name: 'kmb0a1b2c3d',
    libvirt_network: 'vpc-0a1b2c3d4e5f60718',
    is_default: true,
    created_at: '2026-01-02T10:00:00Z',
    updated_at: '2026-01-02T10:00:00Z',
    tags: { team: 'platform' },
  },
  {
    id: 'vpc-0b2c3d4e5f6071890',
    name: 'prod',
    cidr: '172.16.0.0/16',
    mode: 'nat',
    gateway_ip: '172.16.0.1',
    netmask: '255.255.0.0',
    bridge_name: 'kmb0b2c3d4e',
    libvirt_network: 'vpc-0b2c3d4e5f6071890',
    is_default: false,
    created_at: '2026-01-02T11:00:00Z',
    updated_at: '2026-01-02T11:00:00Z',
    tags: {},
  },
]

const INITIAL_SUBNETS: Subnet[] = [
  {
    id: 'subnet-0a1b2c3d4e5f60718',
    vpc_id: 'vpc-0a1b2c3d4e5f60718',
    name: 'default',
    cidr: '10.0.0.0/24',
    total_ips: 256,
    usable_ips: 253,
    allocated_ips: 2,
    is_default: true,
    created_at: '2026-01-02T10:00:00Z',
    updated_at: '2026-01-02T10:00:00Z',
    tags: { team: 'platform' },
  },
  {
    id: 'subnet-0b2c3d4e5f6071890',
    vpc_id: 'vpc-0a1b2c3d4e5f60718',
    name: 'db',
    cidr: '10.0.1.0/24',
    total_ips: 256,
    usable_ips: 253,
    allocated_ips: 1,
    is_default: false,
    created_at: '2026-01-02T10:20:00Z',
    updated_at: '2026-01-02T10:20:00Z',
    tags: {},
  },
  {
    id: 'subnet-0c2d3e4f5a6071819',
    vpc_id: 'vpc-0b2c3d4e5f6071890',
    name: 'app',
    cidr: '172.16.0.0/24',
    total_ips: 256,
    usable_ips: 253,
    allocated_ips: 0,
    is_default: false,
    created_at: '2026-01-02T11:10:00Z',
    updated_at: '2026-01-02T11:10:00Z',
    tags: {},
  },
]

function freshVPCs(): VPC[] {
  return INITIAL_VPCS.map((vpc) => ({ ...vpc, tags: { ...vpc.tags } }))
}

function freshSubnets(): Subnet[] {
  return INITIAL_SUBNETS.map((subnet) => ({ ...subnet, tags: { ...subnet.tags } }))
}

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
let instanceTypes: InstanceType[] = freshInstanceTypes()

function freshInstanceTypes(): InstanceType[] {
  return INITIAL_INSTANCE_TYPES.map((type) => ({ ...type }))
}

/** mockInstanceTypes is the current type table, for assertions. */
export function mockInstanceTypes(): InstanceType[] {
  return instanceTypes
}

/** setMockInstanceTypes replaces the type table, e.g. to start from an empty list. */
export function setMockInstanceTypes(next: InstanceType[]): void {
  instanceTypes = next
}
let tasks: Task[] = freshTasks()
let instances: Instance[] = freshInstances()
let dbInstances: DBInstance[] = freshDBInstances()
let clusters: Cluster[] = freshClusters()
let sparkClusters: SparkCluster[] = freshSparkClusters()
let vpcs: VPC[] = freshVPCs()
let subnets: Subnet[] = freshSubnets()

/** mockImages is the current fixture table, for assertions. */
export function mockImages(): MockImage[] {
  return images
}

/** mockTasks is the current task table, for assertions. */
export function mockTasks(): Task[] {
  return tasks
}

/** setMockTasks replaces the task table, e.g. for a node that has run nothing. */
export function setMockTasks(next: Task[]): void {
  tasks = next
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
 * nextResourceID mints an id like the ones kumod generates (`img-`/`task-`/`i-`
 * plus 17 hex characters), derived from the shared counter so two fixtures minted
 * in the same test cannot collide.
 */
function nextResourceID(
  prefix: 'img' | 'task' | 'i' | 'eni' | 'vpc' | 'subnet' | 'db' | 'eks' | 'ng' | 'spk',
): string {
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
/**
 * The node the capacity report describes: 8 vCPUs over a 4x overcommit, 16 GiB
 * of memory and a 200 GiB instances pool, with one small instance's worth
 * already handed out. The numbers are deliberately unround so a test asserting
 * a formatted figure cannot pass on a coincidence.
 */
function freshCapacity(): components['schemas']['SystemCapacity'] {
  return {
    vcpus: { total: 32, allocated: 3 },
    memory_bytes: { total: 17179869184, allocated: 3221225472 },
    disk_bytes: { total: 214748364800, allocated: 32212254720 },
  }
}

let capacity = freshCapacity()

/** setMockCapacity replaces the capacity report, for the over-allocated case. */
export function setMockCapacity(next: components['schemas']['SystemCapacity']): void {
  capacity = next
}

export function resetMockState(): void {
  refreshTokens = new Map()
  accessTokens = new Map()
  info = { ...info, needs_setup: false }
  keyPairs = freshKeyPairs()
  images = freshImages()
  instanceTypes = freshInstanceTypes()
  tasks = freshTasks()
  instances = freshInstances()
  vpcs = freshVPCs()
  subnets = freshSubnets()
  capacity = freshCapacity()
  buckets = freshBuckets()
  listPageSize = 200
  accessKeys = freshAccessKeys()
  dbInstances = freshDBInstances()
  clusters = freshClusters()
  sparkClusters = freshSparkClusters()
  info = {
    ...info,
    object_storage: { backend: 'fake', ok: true },
    databases: { enabled: true, engines: MOCK_ENGINES },
    kubernetes: { enabled: true, versions: ['1.37'] },
    spark: { enabled: true, versions: ['4.3'], object_storage: true },
  }
}

/** mockSparkClusters reads the Spark cluster fixtures. */
export function mockSparkClusters(): SparkCluster[] {
  return sparkClusters
}

/** setMockSparkClusters replaces the Spark cluster fixtures. */
export function setMockSparkClusters(next: SparkCluster[]): void {
  sparkClusters = next
}

/** setMockSpark switches the Spark module: off, on with no image, no storage. */
export function setMockSpark(spark: SystemInfo['spark']): void {
  info = { ...info, spark }
}

/** mockClusters reads the cluster fixtures. */
export function mockClusters(): Cluster[] {
  return clusters
}

/** setMockClusters replaces the cluster fixtures. */
export function setMockClusters(next: Cluster[]): void {
  clusters = next
}

/** setMockKubernetes switches the Kubernetes module: off, or on with no image. */
export function setMockKubernetes(kubernetes: SystemInfo['kubernetes']): void {
  info = { ...info, kubernetes }
}

/** mockDBInstances reads the DB instance fixtures, e.g. to assert a delete. */
export function mockDBInstances(): DBInstance[] {
  return dbInstances
}

/** setMockDBInstances replaces the fixture table, e.g. to start from none. */
export function setMockDBInstances(next: DBInstance[]): void {
  dbInstances = next
}

/** mockInstances reads the instance fixtures, e.g. to assert a state change. */
export function mockInstances(): Instance[] {
  return instances
}

/** setMockInstances replaces the fixture table, e.g. to start from an empty list. */
export function setMockInstances(next: Instance[]): void {
  instances = next
}

/** findMockInstance is the one fixture row a test is acting on. */
export function findMockInstance(id: string): Instance | undefined {
  return instances.find((instance) => instance.id === id)
}

/** mockVPCs reads the VPC fixtures, e.g. to assert a delete. */
export function mockVPCs(): VPC[] {
  return vpcs
}

/** setMockVPCs replaces the fixture table, e.g. to start from an empty list. */
export function setMockVPCs(next: VPC[]): void {
  vpcs = next
}

/** mockSubnets reads the subnet fixtures, e.g. to assert a delete. */
export function mockSubnets(): Subnet[] {
  return subnets
}

/** setMockSubnets replaces the fixture table, e.g. to start from an empty list. */
export function setMockSubnets(next: Subnet[]): void {
  subnets = next
}

/** findMockSubnet is the one subnet fixture row a test is acting on. */
export function findMockSubnet(id: string): Subnet | undefined {
  return subnets.find((subnet) => subnet.id === id)
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

// ------------------------------------------------------ vpc/subnet helpers ---

/** Cidr is an IPv4 block as its network address and prefix length. */
interface Cidr {
  base: number
  prefix: number
}

/** ipToInt parses a dotted quad into its uint32 value, or null. */
function ipToInt(ip: string): number | null {
  const parts = ip.split('.')
  if (parts.length !== 4) {
    return null
  }
  let value = 0
  for (const part of parts) {
    if (!/^\d{1,3}$/.test(part)) {
      return null
    }
    const octet = Number(part)
    if (octet < 0 || octet > 255) {
      return null
    }
    value = (value << 8) | octet
  }
  return value >>> 0
}

/** intToIp renders a uint32 as a dotted quad. */
function intToIp(value: number): string {
  return `${(value >>> 24) & 0xff}.${(value >>> 16) & 0xff}.${(value >>> 8) & 0xff}.${value & 0xff}`
}

/** prefixNetmask is a prefix length expanded to a dotted-quad netmask. */
function prefixNetmask(prefix: number): string {
  const mask = prefix === 0 ? 0 : (0xffffffff << (32 - prefix)) >>> 0
  return intToIp(mask)
}

/** parseCIDR parses "a.b.c.d/prefix" into its network base and prefix, or null. */
function parseCIDR(cidr: string): Cidr | null {
  const match = /^(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})\/(\d{1,2})$/.exec(cidr)
  if (match === null) {
    return null
  }
  const host = ipToInt(match[1] as string)
  const prefix = Number(match[2])
  if (host === null || prefix < 0 || prefix > 32) {
    return null
  }
  const mask = prefix === 0 ? 0 : (0xffffffff << (32 - prefix)) >>> 0
  return { base: (host & mask) >>> 0, prefix }
}

/** privateRanges is the RFC 1918 address space the API allows for VPCs. */
const privateRanges: ReadonlyArray<Cidr> = [
  { base: 0x0a000000, prefix: 8 }, // 10.0.0.0/8
  { base: 0xac100000, prefix: 12 }, // 172.16.0.0/12
  { base: 0xc0a80000, prefix: 16 }, // 192.168.0.0/16
]

/** isPrivateRange asks whether a block sits entirely inside RFC 1918 space. */
function isPrivateRange(cidr: Cidr): boolean {
  return privateRanges.some((range) => contains(range, cidr))
}

/** contains reports whether `outer` covers every address of `inner`. */
function contains(outer: Cidr, inner: Cidr): boolean {
  if (outer.prefix > inner.prefix) {
    return false
  }
  const mask = outer.prefix === 0 ? 0 : (0xffffffff << (32 - outer.prefix)) >>> 0
  return outer.base === (inner.base & mask) >>> 0
}

/** overlaps reports whether two blocks share at least one address. */
function overlaps(a: Cidr, b: Cidr): boolean {
  const size = (prefix: number): number => (prefix === 0 ? 0x100000000 : 1 << (32 - prefix))
  return a.base < b.base + size(b.prefix) && b.base < a.base + size(a.prefix)
}

/**
 * validateNewVPC mirrors the VPC create rules in
 * `internal/service/vpc/service.go`: an RFC 1123 name free among VPCs, a
 * private /16 to /28 block that nothing else owns. An overlap is a `409`
 * conflict (the caller turns it into one), everything else a field error.
 */
interface VPCValidationResult {
  field: string
  reason: string
}

function validateNewVPC(
  body: components['schemas']['CreateVPCRequest'],
  existing: ReadonlyArray<VPC>,
): VPCValidationResult | null {
  if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(body.name) || body.name.length > 63) {
    return {
      field: 'name',
      reason:
        'must be 1-63 characters of lowercase letters, digits and dashes, starting and ending with a letter or digit',
    }
  }
  if (existing.some((vpc) => vpc.name === body.name)) {
    return { field: 'name', reason: `a VPC named "${body.name}" already exists` }
  }
  if (body.mode !== 'nat' && body.mode !== 'isolated') {
    return { field: 'mode', reason: 'must be "nat" or "isolated"' }
  }
  const parsed = parseCIDR(body.cidr)
  if (parsed === null) {
    return { field: 'cidr', reason: 'must be an IPv4 CIDR block like 10.0.0.0/16' }
  }
  if (!isPrivateRange(parsed)) {
    return { field: 'cidr', reason: 'must be a private RFC 1918 block' }
  }
  if (parsed.prefix < 16 || parsed.prefix > 28) {
    return { field: 'cidr', reason: 'must be a /16 to /28 block' }
  }
  const clash = existing.find((vpc) => overlaps(parsed, parseCIDR(vpc.cidr) as Cidr))
  if (clash !== undefined) {
    // An overlap answers 409 rather than landing on the field: it is a conflict
    // with another resource, not a malformed value.
    return { field: 'cidr', reason: `the CIDR overlaps the VPC "${clash.name}" (${clash.cidr})` }
  }
  return null
}

/**
 * validateNewSubnet mirrors the subnet create rules in
 * `internal/service/subnet/service.go`: the VPC must exist, the name must be
 * unique within it, and the CIDR must sit entirely inside the VPC without
 * overlapping a sibling. An overlap is a `409`, everything else a field error.
 */
function validateNewSubnet(
  body: components['schemas']['CreateSubnetRequest'],
  existing: ReadonlyArray<Subnet>,
  parent: VPC | undefined,
): VPCValidationResult | null {
  if (parent === undefined) {
    return { field: 'vpc_id', reason: `no VPC "${body.vpc_id}"` }
  }
  if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(body.name) || body.name.length > 63) {
    return {
      field: 'name',
      reason:
        'must be 1-63 characters of lowercase letters, digits and dashes, starting and ending with a letter or digit',
    }
  }
  if (existing.some((subnet) => subnet.name === body.name)) {
    return { field: 'name', reason: `a subnet named "${body.name}" already exists in the VPC` }
  }
  const parsed = parseCIDR(body.cidr)
  if (parsed === null) {
    return { field: 'cidr', reason: 'must be an IPv4 CIDR block like 10.0.1.0/24' }
  }
  const parentCidr = parseCIDR(parent.cidr)
  if (parentCidr === null || !contains(parentCidr, parsed)) {
    return {
      field: 'cidr',
      reason: `must sit entirely inside the VPC "${parent.name}" (${parent.cidr})`,
    }
  }
  const clash = existing.find((subnet) => overlaps(parsed, parseCIDR(subnet.cidr) as Cidr))
  if (clash !== undefined) {
    return {
      field: 'cidr',
      reason: `the CIDR overlaps the subnet "${clash.name}" (${clash.cidr})`,
    }
  }
  return null
}

/** subnetAddresses fills in the counts a subnet row carries. */
function subnetAddresses(cidr: string): { total: number; usable: number } {
  const parsed = parseCIDR(cidr)
  const total = parsed === null ? 0 : 2 ** (32 - parsed.prefix)
  // Minus the network address, the broadcast address and the VPC gateway.
  return { total, usable: Math.max(0, total - 3) }
}

/**
 * nextSubnetIP hands out the first free host address of a subnet, past the VPC
 * gateway. It is the mock's answer to the launch allocation transaction: the
 * same subnet always yields the same address for the same rows.
 */
function nextSubnetIP(subnetId: string, cidr: string): string {
  const parsed = parseCIDR(cidr)
  const taken = instances.filter((instance) => instance.subnet_id === subnetId).length
  // Network + gateway + every already-allocated address.
  return intToIp(((parsed?.base ?? 0) + 2 + taken) >>> 0)
}

/** macFromIP derives the guest's fixed MAC from its allocated address. */
function macFromIP(ip: string): string {
  const value = ipToInt(ip) ?? 0
  const hex = (octet: number): string => octet.toString(16).padStart(2, '0')
  return `52:54:00:${hex((value >>> 16) & 0xff)}:${hex((value >>> 8) & 0xff)}:${hex(value & 0xff)}`
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

  http.get('/api/v1/system/capacity', ({ request }) => {
    const user = requireUser(request)
    return user === null ? unauthenticated() : HttpResponse.json(capacity)
  }),

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
      kind: 'user',
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

  // ------------------------------------------------------------- instances ---

  http.get('/api/v1/instance-types', ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    return HttpResponse.json({ items: sortInstanceTypes(instanceTypes) })
  }),

  http.get('/api/v1/instance-types/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    const type = instanceTypes.find((candidate) => candidate.id === String(params.id))
    if (type === undefined) {
      return apiError(404, 'not_found', `no instance type "${String(params.id)}"`)
    }
    return HttpResponse.json({ ...type, used_by: instancesUsingType(type.id) })
  }),

  http.post('/api/v1/instance-types', async ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const body = (await request.json()) as Partial<InstanceType>
    const id = body.id ?? ''
    if (!/^[a-z0-9][a-z0-9._-]{0,63}$/.test(id)) {
      return invalidArgument('id', 'must be 1-64 lowercase letters, digits, ".", "_" or "-"')
    }
    const sizeError = validateInstanceTypeSize(body.vcpus ?? 0, body.memory_mib ?? 0)
    if (sizeError !== null) {
      return invalidArgument(sizeError.field, sizeError.reason)
    }
    if (instanceTypes.some((candidate) => candidate.id === id)) {
      return apiError(409, 'already_exists', `an instance type "${id}" already exists`)
    }
    const created: InstanceType = {
      id,
      vcpus: body.vcpus ?? 0,
      memory_mib: body.memory_mib ?? 0,
      description:
        body.description === undefined || body.description === ''
          ? describeInstanceType(body.vcpus ?? 0, body.memory_mib ?? 0)
          : body.description,
    }
    instanceTypes = [...instanceTypes, created]
    return HttpResponse.json(created, {
      status: 201,
      headers: { Location: `/api/v1/instance-types/${id}` },
    })
  }),

  http.patch('/api/v1/instance-types/:id', async ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const current = instanceTypes.find((candidate) => candidate.id === String(params.id))
    if (current === undefined) {
      return apiError(404, 'not_found', `no instance type "${String(params.id)}"`)
    }
    const body = (await request.json()) as Partial<InstanceType>
    if (
      body.vcpus === undefined &&
      body.memory_mib === undefined &&
      body.description === undefined
    ) {
      return invalidArgument('body', 'must set vcpus, memory_mib or description')
    }
    const next: InstanceType = {
      ...current,
      vcpus: body.vcpus ?? current.vcpus,
      memory_mib: body.memory_mib ?? current.memory_mib,
    }
    const sizeError = validateInstanceTypeSize(next.vcpus, next.memory_mib)
    if (sizeError !== null) {
      return invalidArgument(sizeError.field, sizeError.reason)
    }
    // As in internal/service/instancetype: a generated description follows a
    // new size, a hand-written one is kept.
    if (body.description !== undefined) {
      next.description = body.description
    } else if (current.description === describeInstanceType(current.vcpus, current.memory_mib)) {
      next.description = describeInstanceType(next.vcpus, next.memory_mib)
    }
    instanceTypes = instanceTypes.map((candidate) =>
      candidate.id === current.id ? next : candidate,
    )
    return HttpResponse.json(next)
  }),

  http.delete('/api/v1/instance-types/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const id = String(params.id)
    if (!instanceTypes.some((candidate) => candidate.id === id)) {
      return apiError(404, 'not_found', `no instance type "${id}"`)
    }
    const usedBy = instancesUsingType(id)
    if (usedBy.length > 0) {
      return apiError(409, 'conflict', `instance type "${id}" is in use by ${usedBy.join(', ')}`)
    }
    instanceTypes = instanceTypes.filter((candidate) => candidate.id !== id)
    return new HttpResponse(null, { status: 204 })
  }),

  http.get('/api/v1/instances', ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    const params = new URL(request.url).searchParams
    const query = params.get('q')
    const state = params.get('state')
    const items = instances
      .filter(
        (instance) => query === null || instance.name.toLowerCase().includes(query.toLowerCase()),
      )
      .filter((instance) => state === null || instance.state === state)
      // Newest first, the API's default order.
      .sort((a, b) => b.launched_at.localeCompare(a.launched_at))
    return HttpResponse.json({ items, next_page_token: null })
  }),

  http.get('/api/v1/instances/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    const id = String(params.id)
    const instance = instances.find((candidate) => candidate.id === id)
    if (instance === undefined) {
      return apiError(404, 'not_found', `no instance "${id}"`)
    }
    return HttpResponse.json(instance)
  }),

  // The launch answers 202 with the instance in `pending` and never waits for the
  // boot, which is the whole point of the task (01-api.md §Instances).
  http.post('/api/v1/instances', async ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const body = (await request.json()) as LaunchInstanceRequest
    const fieldError = validateLaunch(body, images, instanceTypes)
    if (fieldError !== null) {
      return invalidArgument(fieldError.field, fieldError.reason)
    }
    // The default subnet of the default VPC is what the backend attaches a
    // launch that names no subnet to (M4b.3). A launch that does name one was
    // vetted by validateLaunch, so the row is guaranteed to exist.
    const targetSubnet =
      body.subnet_id === undefined
        ? subnets.find((subnet) => subnet.is_default)
        : subnets.find((subnet) => subnet.id === body.subnet_id)
    const image = images.find((candidate) => candidate.id === body.image_id) as MockImage
    const preset = instanceTypes.find((candidate) => candidate.id === body.instance_type)
    const task: Task = {
      id: nextResourceID('task'),
      type: 'instance.launch',
      state: 'pending',
      created_at: '2026-01-06T04:00:00Z',
      updated_at: '2026-01-06T04:00:00Z',
    }
    const pair =
      body.key_pair_id === undefined
        ? undefined
        : keyPairs.find((candidate) => candidate.id === body.key_pair_id)
    const privateIp =
      targetSubnet === undefined ? null : nextSubnetIP(targetSubnet.id, targetSubnet.cidr)
    const mac = privateIp === null ? null : macFromIP(privateIp)
    const instance: Instance = {
      id: nextResourceID('i'),
      name: body.name,
      state: 'pending',
      state_reason: null,
      image_id: body.image_id,
      image_name: image.name,
      instance_type: body.instance_type,
      vcpus: preset?.vcpus ?? 1,
      memory_mib: preset?.memory_mib ?? 1024,
      // `min_disk_gb` is optional on the schema, so an image that never declared
      // one falls back to the API's 1 GiB floor.
      root_disk_gb: body.root_disk_gb ?? image.min_disk_gb ?? 1,
      key_pair_id: body.key_pair_id ?? null,
      key_pair_name: pair?.name ?? null,
      private_ip: privateIp,
      mac_address: mac,
      vpc_id: targetSubnet?.vpc_id ?? null,
      subnet_id: targetSubnet?.id ?? null,
      network_interfaces:
        targetSubnet === undefined || privateIp === null || mac === null
          ? []
          : [
              {
                id: nextResourceID('eni'),
                device_index: 0,
                mac_address: mac,
                private_ip: privateIp,
                subnet_id: targetSubnet.id,
                vpc_id: targetSubnet.vpc_id,
              },
            ],
      // `user_data` is echoed to admins only, and the list never carries it.
      user_data: body.user_data ?? null,
      task_id: task.id,
      launched_at: task.created_at,
      terminated_at: null,
      updated_at: task.created_at,
      tags: { ...(body.tags ?? {}) },
    }
    tasks = [task, ...tasks]
    instances = [instance, ...instances]
    if (targetSubnet !== undefined) {
      subnets = subnets.map((subnet) =>
        subnet.id === targetSubnet.id
          ? { ...subnet, allocated_ips: subnet.allocated_ips + 1 }
          : subnet,
      )
    }
    // Launching from an image is what makes deleting it a 409, and attaching a
    // key pair is what makes deleting that a 409 — the same linkage the Go
    // service keeps.
    images = images.map((candidate) =>
      candidate.id === body.image_id
        ? { ...candidate, referenced_by: [...candidate.referenced_by, instance.id] }
        : candidate,
    )
    keyPairs = keyPairs.map((candidate) =>
      body.key_pair_id !== undefined && candidate.id === body.key_pair_id
        ? { ...candidate, used_by: [...candidate.used_by, instance.id] }
        : candidate,
    )
    return HttpResponse.json(instance, {
      status: 202,
      headers: { Location: `/api/v1/tasks/${task.id}` },
    })
  }),

  http.patch('/api/v1/instances/:id', async ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const body = (await request.json()) as components['schemas']['UpdateInstanceRequest']
    const id = String(params.id)
    const instance = instances.find((candidate) => candidate.id === id)
    if (instance === undefined) {
      return apiError(404, 'not_found', `no instance "${id}"`)
    }
    if (body.name !== undefined && body.name !== instance.name) {
      // A terminated instance keeps its name as history, and that history is what
      // frees the name for reuse — so a live instance must still not collide.
      const taken = instances.some(
        (candidate) =>
          candidate.name === body.name && candidate.state !== 'terminated' && candidate.id !== id,
      )
      if (taken) {
        return invalidArgument('name', `an instance named "${body.name}" already exists`)
      }
      if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(body.name)) {
        return invalidArgument(
          'name',
          'must be 1-63 characters of lowercase letters, digits and dashes',
        )
      }
    }
    const updated: Instance = {
      ...instance,
      ...(body.name === undefined ? {} : { name: body.name }),
      ...(body.tags === undefined ? {} : { tags: { ...body.tags } }),
      updated_at: '2026-01-06T05:00:00Z',
    }
    instances = instances.map((candidate) => (candidate.id === id ? updated : candidate))
    return HttpResponse.json(updated)
  }),

  // Every lifecycle action answers 202 and queues a task; the guard table and the
  // state each one lands in live in `lifecycle` below.
  http.post('/api/v1/instances/:id/start', ({ request, params }) => {
    const result = lifecycle(request, String(params.id), 'start')
    return 'response' in result ? result.response : accepted(result.instance)
  }),

  http.post('/api/v1/instances/:id/stop', ({ request, params }) => {
    const result = lifecycle(request, String(params.id), 'stop')
    return 'response' in result ? result.response : accepted(result.instance)
  }),

  http.post('/api/v1/instances/:id/reboot', ({ request, params }) => {
    const result = lifecycle(request, String(params.id), 'reboot')
    return 'response' in result ? result.response : accepted(result.instance)
  }),

  http.delete('/api/v1/instances/:id', ({ request, params }) => {
    const result = lifecycle(request, String(params.id), 'terminate')
    return 'response' in result ? result.response : accepted(result.instance)
  }),

  http.get('/api/v1/instances/:id/console-output', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const id = String(params.id)
    if (instances.some((candidate) => candidate.id === id)) {
      return HttpResponse.json({
        instance_id: id,
        output:
          '[    0.000000] Linux version 6.1.0\n[    1.204321] cloud-init: loading user-data\n',
      })
    }
    return apiError(404, 'not_found', `no instance "${id}"`)
  }),

  // ------------------------------------------------------------------ vpcs ---

  http.get('/api/v1/vpcs', ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    const params = new URL(request.url).searchParams
    const query = params.get('q')
    const items = vpcs
      .filter((vpc) => query === null || vpc.name.toLowerCase().includes(query.toLowerCase()))
      // Newest first, the API's default order.
      .sort((a, b) => b.created_at.localeCompare(a.created_at))
    return HttpResponse.json({ items, next_page_token: null })
  }),

  // Creating a VPC answers 202 with the row and the task that brings the
  // network up; the mock never pretends the network exists, exactly like a
  // healthy backend that has not run the task yet.
  http.post('/api/v1/vpcs', async ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const body = (await request.json()) as components['schemas']['CreateVPCRequest']
    const fieldError = validateNewVPC(body, vpcs)
    if (fieldError !== null) {
      if (fieldError.field === 'cidr' && fieldError.reason.includes('overlaps')) {
        return apiError(409, 'conflict', fieldError.reason)
      }
      return invalidArgument(fieldError.field, fieldError.reason)
    }
    const id = nextResourceID('vpc')
    const parsed = parseCIDR(body.cidr)
    const task: Task = {
      id: nextResourceID('task'),
      type: 'vpc.create',
      resource_id: id,
      state: 'pending',
      created_at: '2026-01-06T07:00:00Z',
      updated_at: '2026-01-06T07:00:00Z',
    }
    const vpc: VPC = {
      id,
      name: body.name,
      cidr: body.cidr,
      mode: body.mode,
      gateway_ip: intToIp(((parsed?.base ?? 0) + 1) >>> 0),
      netmask: prefixNetmask(parsed?.prefix ?? 16),
      // `kmb` plus the first 8 hex of the id, like the real bridge names.
      bridge_name: `kmb${id.slice(4, 12)}`,
      libvirt_network: id,
      is_default: false,
      created_at: task.created_at,
      updated_at: task.created_at,
      tags: { ...body.tags },
    }
    tasks = [task, ...tasks]
    vpcs = [vpc, ...vpcs]
    return HttpResponse.json({ ...vpc, task_id: task.id }, { status: 202 })
  }),

  http.get('/api/v1/vpcs/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    const id = String(params.id)
    const vpc = vpcs.find((candidate) => candidate.id === id)
    if (vpc === undefined) {
      return apiError(404, 'not_found', `no VPC "${id}"`)
    }
    const subnetCount = subnets.filter((subnet) => subnet.vpc_id === id).length
    const instanceCount = instances.filter(
      (instance) => instance.vpc_id === id && instance.state !== 'terminated',
    ).length
    return HttpResponse.json({ ...vpc, subnet_count: subnetCount, instance_count: instanceCount })
  }),

  http.patch('/api/v1/vpcs/:id', async ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const body = (await request.json()) as components['schemas']['UpdateTagsRequest']
    const id = String(params.id)
    const vpc = vpcs.find((candidate) => candidate.id === id)
    if (vpc === undefined) {
      return apiError(404, 'not_found', `no VPC "${id}"`)
    }
    const updated: VPC = { ...vpc, tags: { ...body.tags }, updated_at: '2026-01-06T08:00:00Z' }
    vpcs = vpcs.map((candidate) => (candidate.id === id ? updated : candidate))
    return HttpResponse.json(updated)
  }),

  // Deleting a VPC answers 202 as a task and refuses with a 409 while any
  // subnet — or any instance inside one — still lives in it. The mock removes
  // the row at once, because a VPC has no state the console would render while
  // the task ran.
  http.delete('/api/v1/vpcs/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const id = String(params.id)
    const vpc = vpcs.find((candidate) => candidate.id === id)
    if (vpc === undefined) {
      return apiError(404, 'not_found', `no VPC "${id}"`)
    }
    const inside = subnets.filter((subnet) => subnet.vpc_id === id)
    if (inside.length > 0) {
      return apiError(
        409,
        'conflict',
        `the VPC "${vpc.name}" still contains the subnet${inside.length === 1 ? '' : 's'} ${inside
          .map((subnet) => `"${subnet.name}"`)
          .join(', ')}; delete them first`,
      )
    }
    const hosted = instances.filter(
      (instance) => instance.vpc_id === id && instance.state !== 'terminated',
    )
    if (hosted.length > 0) {
      return apiError(
        409,
        'conflict',
        `the VPC "${vpc.name}" still hosts the instance${hosted.length === 1 ? '' : 's'} ${hosted
          .map((instance) => `"${instance.name}"`)
          .join(', ')}; terminate them first`,
      )
    }
    const task: Task = {
      id: nextResourceID('task'),
      type: 'vpc.delete',
      resource_id: id,
      state: 'pending',
      created_at: '2026-01-06T09:00:00Z',
      updated_at: '2026-01-06T09:00:00Z',
    }
    tasks = [task, ...tasks]
    vpcs = vpcs.filter((candidate) => candidate.id !== id)
    return HttpResponse.json({ ...vpc, task_id: task.id }, { status: 202 })
  }),

  // ---------------------------------------------------------------- subnets ---

  http.get('/api/v1/subnets', ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    const params = new URL(request.url).searchParams
    const vpcFilter = params.get('vpc_id')
    const query = params.get('q')
    const items = subnets
      .filter((subnet) => vpcFilter === null || subnet.vpc_id === vpcFilter)
      .filter((subnet) => query === null || subnet.name.toLowerCase().includes(query.toLowerCase()))
      // Newest first, the API's default order.
      .sort((a, b) => b.created_at.localeCompare(a.created_at))
    return HttpResponse.json({ items, next_page_token: null })
  }),

  // A subnet is a row, so creating one answers 201 with the finished resource.
  http.post('/api/v1/subnets', async ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const body = (await request.json()) as components['schemas']['CreateSubnetRequest']
    const parent = vpcs.find((vpc) => vpc.id === body.vpc_id)
    const siblings = subnets.filter((subnet) => subnet.vpc_id === body.vpc_id)
    const fieldError = validateNewSubnet(body, siblings, parent)
    if (fieldError !== null) {
      if (fieldError.field === 'vpc_id') {
        return apiError(404, 'not_found', fieldError.reason)
      }
      if (fieldError.field === 'cidr' && fieldError.reason.includes('overlaps')) {
        return apiError(409, 'conflict', fieldError.reason)
      }
      return invalidArgument(fieldError.field, fieldError.reason)
    }
    const { total, usable } = subnetAddresses(body.cidr)
    const subnet: Subnet = {
      id: nextResourceID('subnet'),
      vpc_id: body.vpc_id,
      name: body.name,
      cidr: body.cidr,
      total_ips: total,
      usable_ips: usable,
      allocated_ips: 0,
      is_default: false,
      created_at: '2026-01-06T07:30:00Z',
      updated_at: '2026-01-06T07:30:00Z',
      tags: { ...body.tags },
    }
    subnets = [subnet, ...subnets]
    return HttpResponse.json(subnet, { status: 201 })
  }),

  http.get('/api/v1/subnets/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    const id = String(params.id)
    const subnet = subnets.find((candidate) => candidate.id === id)
    if (subnet === undefined) {
      return apiError(404, 'not_found', `no subnet "${id}"`)
    }
    return HttpResponse.json(subnet)
  }),

  http.patch('/api/v1/subnets/:id', async ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const body = (await request.json()) as components['schemas']['UpdateTagsRequest']
    const id = String(params.id)
    const subnet = subnets.find((candidate) => candidate.id === id)
    if (subnet === undefined) {
      return apiError(404, 'not_found', `no subnet "${id}"`)
    }
    const updated: Subnet = {
      ...subnet,
      tags: { ...body.tags },
      updated_at: '2026-01-06T08:00:00Z',
    }
    subnets = subnets.map((candidate) => (candidate.id === id ? updated : candidate))
    return HttpResponse.json(updated)
  }),

  // A subnet an interface still holds an address from is a 409; otherwise the
  // row disappears with a 204.
  http.delete('/api/v1/subnets/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const id = String(params.id)
    const subnet = subnets.find((candidate) => candidate.id === id)
    if (subnet === undefined) {
      return apiError(404, 'not_found', `no subnet "${id}"`)
    }
    if (subnet.allocated_ips > 0) {
      return apiError(
        409,
        'conflict',
        `the subnet "${subnet.name}" still has ${subnet.allocated_ips} allocated address${
          subnet.allocated_ips === 1 ? '' : 'es'
        }; terminate the instances first`,
      )
    }
    subnets = subnets.filter((candidate) => candidate.id !== id)
    return new HttpResponse(null, { status: 204 })
  }),

  // ----------------------------------------------------------- access keys ---

  http.get('/api/v1/s3/access-keys', ({ request }) => {
    if (requireUser(request) === undefined) {
      return unauthenticated()
    }
    const refusal = objectStorageRefusal()
    if (refusal !== null) {
      return refusal
    }
    return HttpResponse.json({
      items: [...accessKeys].sort((a, b) => a.name.localeCompare(b.name)),
    })
  }),

  http.get('/api/v1/s3/access-keys/:id', ({ request, params }) => {
    if (requireUser(request) === undefined) {
      return unauthenticated()
    }
    const key = accessKeys.find((candidate) => candidate.access_key === String(params.id))
    return key === undefined
      ? apiError(404, 'not_found', `no access key "${String(params.id)}"`)
      : HttpResponse.json(key)
  }),

  http.post('/api/v1/s3/access-keys', async ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const body = (await request.json()) as { name?: string; level?: string; buckets?: string[] }
    const refusal = validateAccessKey(body, null)
    if (refusal !== null) {
      return refusal
    }
    const id = `KUMO${String((counter += 1)).padStart(16, 'Q')}`
    const key: AccessKey = {
      access_key: id,
      name: body.name ?? '',
      level: body.level === 'read-write' ? 'read-write' : 'read-only',
      buckets: [...new Set(body.buckets ?? [])].sort(),
      state: 'active',
      created_by: user.username,
      created_at: '2026-01-06T09:00:00Z',
      updated_at: '2026-01-06T09:00:00Z',
    }
    accessKeys = [...accessKeys, key]
    return HttpResponse.json(
      { ...key, secret_key: `mock-secret-${id}` },
      {
        status: 201,
        headers: { Location: `/api/v1/s3/access-keys/${id}`, 'Cache-Control': 'no-store' },
      },
    )
  }),

  http.patch('/api/v1/s3/access-keys/:id', async ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const current = accessKeys.find((candidate) => candidate.access_key === String(params.id))
    if (current === undefined) {
      return apiError(404, 'not_found', `no access key "${String(params.id)}"`)
    }
    const body = (await request.json()) as { name?: string; level?: string; buckets?: string[] }
    const next: AccessKey = {
      ...current,
      name: body.name ?? current.name,
      level:
        body.level === undefined
          ? current.level
          : body.level === 'read-write'
            ? 'read-write'
            : 'read-only',
      buckets: body.buckets === undefined ? current.buckets : [...new Set(body.buckets)].sort(),
    }
    const refusal = validateAccessKey(next, current.access_key)
    if (refusal !== null) {
      return refusal
    }
    accessKeys = accessKeys.map((candidate) => (candidate === current ? next : candidate))
    return HttpResponse.json(next)
  }),

  http.delete('/api/v1/s3/access-keys/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    if (!accessKeys.some((candidate) => candidate.access_key === String(params.id))) {
      return apiError(404, 'not_found', `no access key "${String(params.id)}"`)
    }
    accessKeys = accessKeys.filter((candidate) => candidate.access_key !== String(params.id))
    return new HttpResponse(null, { status: 204 })
  }),

  // -------------------------------------------------------------- clusters ---

  http.get('/api/v1/eks/clusters', ({ request }) => {
    if (requireUser(request) === undefined) {
      return unauthenticated()
    }
    const refusal = kubernetesRefusal()
    if (refusal !== null) {
      return refusal
    }
    const items = [...clusters].sort((a, b) => b.created_at.localeCompare(a.created_at))
    return HttpResponse.json({ items, next_page_token: null })
  }),

  http.get('/api/v1/eks/clusters/:id', ({ request, params }) => {
    if (requireUser(request) === undefined) {
      return unauthenticated()
    }
    const refusal = kubernetesRefusal()
    if (refusal !== null) {
      return refusal
    }
    const found = clusters.find((candidate) => candidate.id === String(params.id))
    if (found === undefined) {
      return apiError(404, 'not_found', `not found: no cluster "${String(params.id)}"`)
    }
    return HttpResponse.json(found)
  }),

  http.post('/api/v1/eks/clusters', async ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = kubernetesRefusal()
    if (refusal !== null) {
      return refusal
    }
    if (info.kubernetes.versions.length === 0) {
      return apiError(409, 'no_engine_image', 'conflict: no available Kubernetes engine image')
    }
    const body = (await request.json()) as Partial<{
      name: string
      kubernetes_version: string
      control_plane_type: string
      node_type: string
      node_count: number
    }>
    const name = body.name ?? ''
    if (!/^[a-z]([-a-z0-9]*[a-z0-9])?$/.test(name)) {
      return invalidArgument('name', 'must be a DNS label of 1-40 characters')
    }
    if (clusters.some((candidate) => candidate.name === name)) {
      return apiError(
        409,
        'already_exists',
        `already exists: a cluster named "${name}" already exists`,
      )
    }
    const count = body.node_count ?? 2
    const groupID = nextResourceID('ng')
    const created: Cluster = {
      id: nextResourceID('eks'),
      name,
      kubernetes_version: body.kubernetes_version ?? '1.37',
      state: 'creating',
      state_reason: null,
      endpoint: null,
      vpc_id: 'vpc-0a1b2c3d4e5f60718',
      subnet_id: 'subnet-0a1b2c3d4e5f60718',
      pod_cidr: '100.64.0.0/16',
      service_cidr: '100.96.0.0/16',
      control_plane_type: body.control_plane_type ?? 'kumo.medium',
      ca_certificate: '-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n',
      node_groups: [
        {
          id: groupID,
          name: 'default',
          instance_type: body.node_type ?? 'kumo.medium',
          root_disk_gb: 8,
          desired_size: count,
        },
      ],
      nodes: [
        clusterNode(`${name}-cp-1`, 'control-plane', '192.168.100.40', null),
        ...Array.from({ length: count }, (_, i) =>
          clusterNode(`${name}-default-${i + 1}`, 'worker', `192.168.100.${41 + i}`, null),
        ),
      ],
      task_id: nextResourceID('task'),
      created_at: '2026-01-07T10:00:00Z',
      updated_at: '2026-01-07T10:00:00Z',
    }
    clusters = [created, ...clusters]
    return HttpResponse.json(created, {
      status: 202,
      headers: { Location: `/api/v1/tasks/${String(created.task_id)}` },
    })
  }),

  http.patch('/api/v1/eks/clusters/:id/node-groups/:ng', async ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = kubernetesRefusal()
    if (refusal !== null) {
      return refusal
    }
    const found = clusters.find((candidate) => candidate.id === String(params.id))
    const group = found?.node_groups.find((candidate) => candidate.id === String(params.ng))
    if (found === undefined || group === undefined) {
      return apiError(404, 'not_found', `not found: no node group "${String(params.ng)}"`)
    }
    const body = (await request.json()) as { desired_size?: number }
    const size = body.desired_size ?? 0
    if (!Number.isInteger(size) || size < 1 || size > 10) {
      return invalidArgument('desired_size', 'must be between 1 and 10')
    }
    if (found.state !== 'active' && found.state !== 'degraded') {
      return apiError(409, 'conflict', `conflict: cluster "${found.id}" is ${found.state}`)
    }
    if (size === group.desired_size) {
      return HttpResponse.json(found)
    }
    const moved: Cluster = {
      ...found,
      state: 'updating',
      node_groups: found.node_groups.map((candidate) =>
        candidate.id === group.id ? { ...candidate, desired_size: size } : candidate,
      ),
      task_id: nextResourceID('task'),
    }
    clusters = clusters.map((candidate) => (candidate.id === found.id ? moved : candidate))
    return HttpResponse.json(moved, {
      status: 202,
      headers: { Location: `/api/v1/tasks/${String(moved.task_id)}` },
    })
  }),

  http.post('/api/v1/eks/clusters/:id/kubeconfig', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    const refusal = kubernetesRefusal()
    if (refusal !== null) {
      return refusal
    }
    const found = clusters.find((candidate) => candidate.id === String(params.id))
    if (found === undefined) {
      return apiError(404, 'not_found', `not found: no cluster "${String(params.id)}"`)
    }
    if (found.endpoint === null || found.endpoint === undefined) {
      return apiError(409, 'conflict', `conflict: cluster "${found.id}" is ${found.state}`)
    }
    const group = user.role === 'admin' ? 'kumo:admins' : 'kumo:viewers'
    return HttpResponse.json(
      {
        kubeconfig: `apiVersion: v1\nkind: Config\nclusters:\n- name: ${found.name}\n  cluster:\n    server: ${found.endpoint}\nusers:\n- name: ${user.username}\n`,
        group,
        expires_at: '2026-02-06T10:00:00Z',
      },
      { headers: { 'Cache-Control': 'no-store' } },
    )
  }),

  http.delete('/api/v1/eks/clusters/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = kubernetesRefusal()
    if (refusal !== null) {
      return refusal
    }
    const found = clusters.find((candidate) => candidate.id === String(params.id))
    if (found === undefined) {
      return apiError(404, 'not_found', `not found: no cluster "${String(params.id)}"`)
    }
    if (found.state === 'deleting') {
      return apiError(409, 'conflict', `conflict: cluster "${found.id}" is already being deleted`)
    }
    const deleting: Cluster = { ...found, state: 'deleting', task_id: nextResourceID('task') }
    clusters = clusters.map((candidate) => (candidate.id === found.id ? deleting : candidate))
    return HttpResponse.json(deleting, {
      status: 202,
      headers: { Location: `/api/v1/tasks/${String(deleting.task_id)}` },
    })
  }),

  // --------------------------------------------------------- spark clusters ---

  http.get('/api/v1/spark/clusters', ({ request }) => {
    if (requireUser(request) === undefined) {
      return unauthenticated()
    }
    const refusal = sparkRefusal()
    if (refusal !== null) {
      return refusal
    }
    const items = [...sparkClusters].sort((a, b) => b.created_at.localeCompare(a.created_at))
    return HttpResponse.json({ items, next_page_token: null })
  }),

  http.get('/api/v1/spark/clusters/:id', ({ request, params }) => {
    if (requireUser(request) === undefined) {
      return unauthenticated()
    }
    const refusal = sparkRefusal()
    if (refusal !== null) {
      return refusal
    }
    const found = sparkClusters.find((candidate) => candidate.id === String(params.id))
    if (found === undefined) {
      return apiError(404, 'not_found', `not found: no Spark cluster "${String(params.id)}"`)
    }
    return HttpResponse.json(found)
  }),

  http.post('/api/v1/spark/clusters', async ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = sparkRefusal()
    if (refusal !== null) {
      return refusal
    }
    if (info.spark.versions.length === 0) {
      return apiError(409, 'no_engine_image', 'conflict: no available Spark engine image')
    }
    const body = (await request.json()) as Partial<{
      name: string
      spark_version: string
      master_type: string
      worker_type: string
      worker_count: number
      object_storage: boolean
      buckets: string[]
      event_log_bucket: string
    }>
    const name = body.name ?? ''
    if (!/^[a-z]([-a-z0-9]*[a-z0-9])?$/.test(name)) {
      return invalidArgument('name', 'must be a DNS label of 1-40 characters')
    }
    if (sparkClusters.some((candidate) => candidate.name === name)) {
      return apiError(
        409,
        'already_exists',
        `already exists: a Spark cluster named "${name}" already exists`,
      )
    }
    const storage = body.object_storage === true
    const chosen = storage ? (body.buckets ?? []) : []
    if (storage && !info.spark.object_storage) {
      return invalidArgument(
        'object_storage',
        'object storage is off on this node (KUMO_OBJECTSTORE)',
      )
    }
    for (const bucket of chosen) {
      if (findBucket(bucket) === undefined) {
        return invalidArgument('buckets', `no bucket "${bucket}" exists`)
      }
    }
    const count = body.worker_count ?? 2
    const created: SparkCluster = {
      id: nextResourceID('spk'),
      name,
      spark_version: body.spark_version ?? '4.3',
      state: 'creating',
      state_reason: null,
      endpoint: null,
      master_ui_url: null,
      history_url: null,
      vpc_id: 'vpc-0a1b2c3d4e5f60718',
      subnet_id: 'subnet-0a1b2c3d4e5f60718',
      master_type: body.master_type ?? 'kumo.medium',
      object_storage: storage,
      buckets: chosen,
      event_log_bucket: storage ? (body.event_log_bucket ?? null) : null,
      node_groups: [
        {
          id: nextResourceID('ng'),
          name: 'default',
          instance_type: body.worker_type ?? 'kumo.large',
          root_disk_gb: 8,
          desired_size: count,
        },
      ],
      nodes: [
        sparkNode(`${name}-master`, 'master', '192.168.100.70', null),
        ...Array.from({ length: count }, (_, i) =>
          sparkNode(`${name}-default-${i + 1}`, 'worker', `192.168.100.${71 + i}`, null),
        ),
      ],
      task_id: nextResourceID('task'),
      created_at: '2026-01-07T10:00:00Z',
      updated_at: '2026-01-07T10:00:00Z',
    }
    sparkClusters = [created, ...sparkClusters]
    return HttpResponse.json(created, {
      status: 202,
      headers: { Location: `/api/v1/tasks/${String(created.task_id)}` },
    })
  }),

  http.patch('/api/v1/spark/clusters/:id/node-groups/:ng', async ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const found = sparkClusters.find((candidate) => candidate.id === String(params.id))
    const group = found?.node_groups.find((candidate) => candidate.id === String(params.ng))
    if (found === undefined || group === undefined) {
      return apiError(404, 'not_found', `not found: no node group "${String(params.ng)}"`)
    }
    const body = (await request.json()) as { desired_size?: number }
    const size = body.desired_size ?? 0
    if (!Number.isInteger(size) || size < 1 || size > 10) {
      return invalidArgument('desired_size', 'must be between 1 and 10')
    }
    if (found.state !== 'active' && found.state !== 'degraded') {
      return apiError(409, 'conflict', `conflict: cluster "${found.id}" is ${found.state}`)
    }
    if (size === group.desired_size) {
      return HttpResponse.json(found)
    }
    const moved: SparkCluster = {
      ...found,
      state: 'updating',
      node_groups: found.node_groups.map((candidate) =>
        candidate.id === group.id ? { ...candidate, desired_size: size } : candidate,
      ),
      task_id: nextResourceID('task'),
    }
    sparkClusters = sparkClusters.map((candidate) =>
      candidate.id === found.id ? moved : candidate,
    )
    return HttpResponse.json(moved, {
      status: 202,
      headers: { Location: `/api/v1/tasks/${String(moved.task_id)}` },
    })
  }),

  http.post('/api/v1/spark/clusters/:id/client-config', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const found = sparkClusters.find((candidate) => candidate.id === String(params.id))
    if (found === undefined) {
      return apiError(404, 'not_found', `not found: no Spark cluster "${String(params.id)}"`)
    }
    if (found.endpoint === null || found.endpoint === undefined) {
      return apiError(409, 'conflict', `conflict: cluster "${found.id}" is ${found.state}`)
    }
    return HttpResponse.json(
      {
        client_config: `spark.master ${found.endpoint}\nspark.authenticate true\nspark.authenticate.secret fixture\n`,
        filename: `kumo-${found.id}.conf`,
      },
      { headers: { 'Cache-Control': 'no-store' } },
    )
  }),

  http.delete('/api/v1/spark/clusters/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const found = sparkClusters.find((candidate) => candidate.id === String(params.id))
    if (found === undefined) {
      return apiError(404, 'not_found', `not found: no Spark cluster "${String(params.id)}"`)
    }
    if (found.state === 'deleting') {
      return apiError(409, 'conflict', `conflict: cluster "${found.id}" is already being deleted`)
    }
    const deleting: SparkCluster = { ...found, state: 'deleting', task_id: nextResourceID('task') }
    sparkClusters = sparkClusters.map((candidate) =>
      candidate.id === found.id ? deleting : candidate,
    )
    return HttpResponse.json(deleting, {
      status: 202,
      headers: { Location: `/api/v1/tasks/${String(deleting.task_id)}` },
    })
  }),

  // ---------------------------------------------------------- db instances ---

  http.get('/api/v1/rds/db-instances', ({ request }) => {
    if (requireUser(request) === undefined) {
      return unauthenticated()
    }
    const refusal = databasesRefusal()
    if (refusal !== null) {
      return refusal
    }
    const items = [...dbInstances].sort((a, b) => b.created_at.localeCompare(a.created_at))
    return HttpResponse.json({ items, next_page_token: null })
  }),

  http.get('/api/v1/rds/db-instances/:id', ({ request, params }) => {
    if (requireUser(request) === undefined) {
      return unauthenticated()
    }
    const refusal = databasesRefusal()
    if (refusal !== null) {
      return refusal
    }
    const found = dbInstances.find((candidate) => candidate.id === String(params.id))
    if (found === undefined) {
      return apiError(404, 'not_found', `no DB instance "${String(params.id)}"`)
    }
    return HttpResponse.json(found)
  }),

  http.post('/api/v1/rds/db-instances', async ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = databasesRefusal()
    if (refusal !== null) {
      return refusal
    }
    if (info.databases.engines.length === 0) {
      return apiError(
        409,
        'no_engine_image',
        'conflict: no engine image is available on this node (engine postgres)',
      )
    }

    const body = (await request.json()) as Partial<{
      identifier: string
      engine_version: string
      instance_type: string
      storage_gb: number
      admin_username: string
      admin_password: string
      database_name: string
    }>
    const identifier = body.identifier ?? ''
    if (!/^[a-z]([-a-z0-9]*[a-z0-9])?$/.test(identifier)) {
      return invalidArgument('identifier', 'must be a DNS label of 1-60 characters')
    }
    if (dbInstances.some((candidate) => candidate.identifier === identifier)) {
      return apiError(409, 'already_exists', `a DB instance named "${identifier}" already exists`)
    }
    // The server refuses a password it cannot derive from, and the form is
    // supposed to have caught it first: the mock enforces it so a test can
    // prove the round trip is not what the field error depends on.
    const password = body.admin_password ?? ''
    if (password.length < 10) {
      return invalidArgument('admin_password', 'must be at least 10 characters')
    }

    const created: DBInstance = {
      id: nextResourceID('db'),
      identifier,
      state: 'creating',
      state_reason: null,
      engine: 'postgres',
      engine_version: body.engine_version ?? '18',
      image_id: 'img-0a1b2c3d4e5f60718',
      instance_id: nextResourceID('i'),
      instance_type: body.instance_type ?? 'kumo.small',
      vcpus: 1,
      memory_mib: 2048,
      storage_gb: body.storage_gb ?? 20,
      port: 5432,
      admin_username: body.admin_username ?? 'app',
      database_name: body.database_name ?? 'app',
      endpoint: null,
      task_id: nextResourceID('task'),
      created_at: '2026-01-06T10:00:00Z',
      updated_at: '2026-01-06T10:00:00Z',
    }
    dbInstances = [created, ...dbInstances]
    tasks = [
      {
        id: created.task_id ?? nextResourceID('task'),
        type: 'db.create',
        resource_id: created.id,
        state: 'pending',
        progress: 0,
        error: null,
        created_at: created.created_at,
        updated_at: created.created_at,
      },
      ...tasks,
    ]
    return HttpResponse.json(created, {
      status: 202,
      headers: { Location: `/api/v1/tasks/${String(created.task_id)}` },
    })
  }),

  http.post('/api/v1/rds/db-instances/:id/:action', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = databasesRefusal()
    if (refusal !== null) {
      return refusal
    }
    const found = dbInstances.find((candidate) => candidate.id === String(params.id))
    if (found === undefined) {
      return apiError(404, 'not_found', `no DB instance "${String(params.id)}"`)
    }

    const action = String(params.action)
    const allowed: Record<string, { from: DBInstance['state'][]; next: DBInstance['state'] }> = {
      stop: { from: ['available'], next: 'stopping' },
      start: { from: ['stopped'], next: 'starting' },
      reboot: { from: ['available'], next: 'rebooting' },
    }
    const rule = allowed[action]
    if (rule === undefined) {
      return apiError(404, 'not_found', `POST /rds/db-instances/${found.id}/${action}`)
    }
    if (!rule.from.includes(found.state)) {
      return apiError(
        409,
        'conflict',
        `the DB instance "${found.id}" is ${found.state}, and ${action} only applies to a ${rule.from.join(' or ')} one`,
      )
    }

    const moved: DBInstance = { ...found, state: rule.next, task_id: nextResourceID('task') }
    dbInstances = dbInstances.map((candidate) => (candidate.id === found.id ? moved : candidate))
    return HttpResponse.json(moved, {
      status: 202,
      headers: { Location: `/api/v1/tasks/${String(moved.task_id)}` },
    })
  }),

  http.delete('/api/v1/rds/db-instances/:id', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = databasesRefusal()
    if (refusal !== null) {
      return refusal
    }
    const found = dbInstances.find((candidate) => candidate.id === String(params.id))
    if (found === undefined) {
      return apiError(404, 'not_found', `no DB instance "${String(params.id)}"`)
    }
    if (found.state === 'deleting') {
      return apiError(409, 'conflict', `the DB instance "${found.id}" is already being deleted`)
    }
    const deleting: DBInstance = { ...found, state: 'deleting', task_id: nextResourceID('task') }
    dbInstances = dbInstances.map((candidate) => (candidate.id === found.id ? deleting : candidate))
    return HttpResponse.json(deleting, {
      status: 202,
      headers: { Location: `/api/v1/tasks/${String(deleting.task_id)}` },
    })
  }),

  // --------------------------------------------------------------- buckets ---

  http.get('/api/v1/s3/buckets', ({ request }) => {
    if (requireUser(request) === undefined) {
      return unauthenticated()
    }
    const refusal = objectStorageRefusal()
    if (refusal !== null) {
      return refusal
    }
    const items = [...buckets]
      .map((entry) => entry.bucket)
      .sort((a, b) => a.name.localeCompare(b.name))
    return HttpResponse.json({ items })
  }),

  http.get('/api/v1/s3/buckets/:name', ({ request, params }) => {
    if (requireUser(request) === undefined) {
      return unauthenticated()
    }
    const refusal = objectStorageRefusal()
    if (refusal !== null) {
      return refusal
    }
    const entry = buckets.find((candidate) => candidate.bucket.name === String(params.name))
    if (entry === undefined) {
      return apiError(404, 'not_found', `no bucket "${String(params.name)}"`)
    }
    return HttpResponse.json(bucketDetail(entry))
  }),

  http.post('/api/v1/s3/buckets', async ({ request }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = objectStorageRefusal()
    if (refusal !== null) {
      return refusal
    }
    const body = (await request.json()) as {
      name?: string
      versioning?: boolean
      tags?: Record<string, string>
    }
    const name = body.name ?? ''
    if (!/^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(name) || name.includes('..')) {
      return invalidArgument('name', 'must be 3-63 lowercase letters, digits, "." or "-"')
    }
    if (buckets.some((candidate) => candidate.bucket.name === name)) {
      return apiError(
        409,
        'already_exists',
        `already exists: a bucket named "${name}" already exists`,
      )
    }
    const entry: MockBucket = {
      bucket: {
        name,
        state: 'available',
        created_at: '2026-01-06T09:00:00Z',
        created_by: user.username,
        tags: body.tags ?? {},
      },
      versioning: body.versioning === true ? 'enabled' : 'off',
      objects: [],
    }
    buckets = [...buckets, entry]
    return HttpResponse.json(bucketDetail(entry), {
      status: 201,
      headers: { Location: `/api/v1/s3/buckets/${name}` },
    })
  }),

  http.patch('/api/v1/s3/buckets/:name', async ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = objectStorageRefusal()
    if (refusal !== null) {
      return refusal
    }
    const current = buckets.find((candidate) => candidate.bucket.name === String(params.name))
    if (current === undefined) {
      return apiError(404, 'not_found', `no bucket "${String(params.name)}"`)
    }
    if (current.bucket.state === 'deleting') {
      return apiError(409, 'conflict', `bucket "${current.bucket.name}" is being deleted`)
    }
    const body = (await request.json()) as { versioning?: boolean; tags?: Record<string, string> }
    if (body.versioning === undefined && body.tags === undefined) {
      return invalidArgument('body', 'must set versioning or tags')
    }
    // As in internal/service/bucket: off on a never-versioned bucket stays off.
    let versioning = current.versioning
    if (body.versioning === true) {
      versioning = 'enabled'
    } else if (body.versioning === false && current.versioning !== 'off') {
      versioning = 'suspended'
    }
    const next: MockBucket = {
      ...current,
      versioning,
      bucket: { ...current.bucket, tags: body.tags ?? current.bucket.tags },
    }
    buckets = buckets.map((candidate) => (candidate === current ? next : candidate))
    return HttpResponse.json(bucketDetail(next))
  }),

  http.delete('/api/v1/s3/buckets/:name', ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = objectStorageRefusal()
    if (refusal !== null) {
      return refusal
    }
    const name = String(params.name)
    const current = buckets.find((candidate) => candidate.bucket.name === name)
    if (current === undefined) {
      return apiError(404, 'not_found', `no bucket "${name}"`)
    }
    const force = new URL(request.url).searchParams.get('force') === 'true'
    if (!force) {
      if (current.bucket.state === 'deleting') {
        return apiError(409, 'conflict', `bucket "${name}" is already being deleted`)
      }
      if (current.objects.length > 0) {
        return apiError(
          409,
          'conflict',
          `conflict: bucket "${name}" still holds objects or old versions; delete them first, or delete the bucket with force`,
        )
      }
      buckets = buckets.filter((candidate) => candidate !== current)
      return new HttpResponse(null, { status: 204 })
    }
    const task: Task = {
      id: nextResourceID('task'),
      type: 'bucket.delete',
      resource_id: `bucket/${name}`,
      state: 'pending',
      created_at: '2026-01-06T09:00:00Z',
      updated_at: '2026-01-06T09:00:00Z',
    }
    tasks = [task, ...tasks]
    const deleting: MockBucket = { ...current, bucket: { ...current.bucket, state: 'deleting' } }
    buckets = buckets.map((candidate) => (candidate === current ? deleting : candidate))
    return HttpResponse.json(
      { bucket: deleting.bucket, task_id: task.id },
      { status: 202, headers: { Location: `/api/v1/tasks/${task.id}` } },
    )
  }),

  http.get('/api/v1/s3/buckets/:name/objects', ({ request, params }) => {
    if (requireUser(request) === undefined) {
      return unauthenticated()
    }
    const refusal = objectStorageRefusal()
    if (refusal !== null) {
      return refusal
    }
    const entry = findBucket(String(params.name))
    if (entry === undefined) {
      return apiError(404, 'not_found', `no bucket "${String(params.name)}"`)
    }
    const query = new URL(request.url).searchParams
    const limit = Number(query.get('limit') ?? listPageSize)
    return HttpResponse.json(
      listFolder(entry, query.get('prefix') ?? '', query.get('cursor'), limit),
    )
  }),

  http.post('/api/v1/s3/buckets/:name/uploads', async ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = objectStorageRefusal()
    if (refusal !== null) {
      return refusal
    }
    const { refusal: bucketRefusal } = writableBucket(String(params.name))
    if (bucketRefusal !== undefined) {
      return bucketRefusal
    }
    const body = (await request.json()) as { key?: string; content_type?: string }
    const key = body.key ?? ''
    if (key === '' || key.startsWith('/') || key.endsWith('/')) {
      return invalidArgument('key', 'must be 1-1024 bytes, not starting or ending with "/"')
    }
    return HttpResponse.json({
      url: `${MOCK_S3_ENDPOINT}/${String(params.name)}/${encodeKey(key)}?X-Amz-Signature=mock`,
      method: 'PUT',
      headers: { 'Content-Type': body.content_type ?? 'application/octet-stream' },
      expires_at: '2026-01-06T09:15:00Z',
    })
  }),

  http.post('/api/v1/s3/buckets/:name/downloads', async ({ request, params }) => {
    if (requireUser(request) === undefined) {
      return unauthenticated()
    }
    const refusal = objectStorageRefusal()
    if (refusal !== null) {
      return refusal
    }
    const entry = findBucket(String(params.name))
    const body = (await request.json()) as { key?: string }
    const key = body.key ?? ''
    if (entry === undefined || !entry.objects.some((object) => object.key === key)) {
      return apiError(404, 'not_found', `no object "${key}"`)
    }
    return HttpResponse.json({
      url: `${MOCK_S3_ENDPOINT}/${entry.bucket.name}/${encodeKey(key)}?X-Amz-Signature=mock`,
      method: 'GET',
      headers: {},
      expires_at: '2026-01-06T09:15:00Z',
    })
  }),

  http.post('/api/v1/s3/buckets/:name/folders', async ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = objectStorageRefusal()
    if (refusal !== null) {
      return refusal
    }
    const { entry, refusal: bucketRefusal } = writableBucket(String(params.name))
    if (bucketRefusal !== undefined) {
      return bucketRefusal
    }
    const body = (await request.json()) as { prefix?: string }
    const folder = `${(body.prefix ?? '').replace(/\/+$/, '')}/`
    if (folder === '/' || folder.startsWith('/')) {
      return invalidArgument('prefix', 'must name a folder')
    }
    if (!entry.objects.some((object) => object.key === folder)) {
      entry.objects = [...entry.objects, mockObject(folder, 0)]
    }
    return HttpResponse.json({ prefix: folder }, { status: 201 })
  }),

  http.post('/api/v1/s3/buckets/:name/objects/delete', async ({ request, params }) => {
    const user = requireUser(request)
    if (user === undefined) {
      return unauthenticated()
    }
    if (user.role !== 'admin') {
      return forbidden()
    }
    const refusal = objectStorageRefusal()
    if (refusal !== null) {
      return refusal
    }
    const { entry, refusal: bucketRefusal } = writableBucket(String(params.name))
    if (bucketRefusal !== undefined) {
      return bucketRefusal
    }
    const body = (await request.json()) as { keys?: string[] }
    const keys = new Set(body.keys ?? [])
    if (keys.size === 0) {
      return invalidArgument('keys', 'must list between 1 and 1000 keys')
    }
    // A folder key removes only its marker, as in internal/service/bucket.
    entry.objects = entry.objects.filter((object) => !keys.has(object.key))
    return new HttpResponse(null, { status: 204 })
  }),

  // The object store itself: a presigned PUT lands here, the way the browser
  // sends it to RustFS.
  http.put(`${MOCK_S3_ENDPOINT}/:bucket/*`, async ({ request, params }) => {
    const entry = findBucket(String(params.bucket))
    if (entry === undefined) {
      return new HttpResponse('<Error><Code>NoSuchBucket</Code></Error>', { status: 404 })
    }
    const url = new URL(request.url)
    const key = decodeURIComponent(url.pathname.slice(`/${entry.bucket.name}/`.length))
    const body = await request.arrayBuffer()
    const object: MockObject = {
      key,
      size_bytes: body.byteLength,
      last_modified: '2026-01-06T09:05:00Z',
      content_type: request.headers.get('Content-Type') ?? 'application/octet-stream',
    }
    entry.objects = [...entry.objects.filter((candidate) => candidate.key !== key), object]
    return new HttpResponse(null, { status: 200, headers: { ETag: '"mock-etag"' } })
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

/** sortInstanceTypes is the API's order: memory, then vCPUs, then id. */
function sortInstanceTypes(types: InstanceType[]): InstanceType[] {
  return [...types].sort(
    (a, b) => a.memory_mib - b.memory_mib || a.vcpus - b.vcpus || a.id.localeCompare(b.id),
  )
}

/**
 * instancesUsingType is `used_by`: every instance row referencing the type,
 * terminated ones included, as the foreign key in the backend counts them.
 */
function instancesUsingType(id: string): string[] {
  return instances
    .filter((instance) => instance.instance_type === id)
    .map((instance) => instance.id)
}

/** validateInstanceTypeSize mirrors the bounds of internal/service/instancetype. */
function validateInstanceTypeSize(
  vcpus: number,
  memoryMiB: number,
): { field: string; reason: string } | null {
  if (!Number.isInteger(vcpus) || vcpus < 1 || vcpus > 256) {
    return { field: 'vcpus', reason: 'must be between 1 and 256' }
  }
  if (!Number.isInteger(memoryMiB) || memoryMiB < 256 || memoryMiB > 1048576) {
    return { field: 'memory_mib', reason: 'must be between 256 and 1048576' }
  }
  return null
}

/** describeInstanceType is the generated description of a size. */
function describeInstanceType(vcpus: number, memoryMiB: number): string {
  return memoryMiB % 1024 === 0
    ? `${vcpus} vCPU, ${memoryMiB / 1024} GiB`
    : `${vcpus} vCPU, ${memoryMiB} MiB`
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

/**
 * TRANSITIONS is the lifecycle guard table the Go service enforces in
 * `internal/service/instance/lifecycle.go`. The mock answers the same `409` for the
 * same states, because a console that only ever sees the happy path against mocks
 * will render a dropdown the API refuses.
 */
const TRANSITIONS: Record<
  'start' | 'stop' | 'reboot' | 'terminate',
  ReadonlySet<Instance['state']>
> = {
  start: new Set(['stopped']),
  stop: new Set(['running', 'stopping']),
  reboot: new Set(['running']),
  terminate: new Set(['pending', 'running', 'stopping', 'stopped', 'terminating', 'error']),
}

/**
 * lifecycle queues one action, applying the guard table and the transition the
 * backend would apply. It returns either an `HttpResponse` or the updated row, so
 * each handler below stays a couple of lines.
 */
type LifecycleAction = keyof typeof TRANSITIONS

/** The state each action moves the row to, for the 202 the console renders. */
const NEXT_STATE: Record<LifecycleAction, Instance['state']> = {
  start: 'pending',
  stop: 'stopping',
  reboot: 'running',
  terminate: 'terminating',
}

/** PAST_TENSE is the action as a past participle, for the 409 message. */
const PAST_TENSE: Record<LifecycleAction, string> = {
  start: 'started',
  stop: 'stopped',
  reboot: 'rebooted',
  terminate: 'terminated',
}

function lifecycle(
  request: Request,
  id: string,
  action: LifecycleAction,
): { response: ReturnType<typeof HttpResponse.json> } | { instance: Instance } {
  const user = requireUser(request)
  if (user === undefined) {
    return { response: unauthenticated() }
  }
  if (user.role !== 'admin') {
    return { response: forbidden() }
  }
  const instance = instances.find((candidate) => candidate.id === id)
  if (instance === undefined) {
    return { response: apiError(404, 'not_found', `no instance "${id}"`) }
  }
  if (!TRANSITIONS[action].has(instance.state)) {
    return {
      response: apiError(
        409,
        'conflict',
        `the instance "${instance.name}" is ${instance.state} and cannot be ${PAST_TENSE[action]}`,
      ),
    }
  }

  const task: Task = {
    id: nextResourceID('task'),
    type: `instance.${action}`,
    resource_id: id,
    state: 'pending',
    created_at: '2026-01-06T06:00:00Z',
    updated_at: '2026-01-06T06:00:00Z',
  }
  tasks = [task, ...tasks]
  // The state an action moves to: stop lands in `stopping` and terminate in
  // `terminating`, both of which the reconciler later settles. Start, stop and
  // reboot leave the instance alone in the mock — the work is a task's job, and a
  // mock that pretended to boot a VM would be claiming something it cannot do.
  const updated: Instance = {
    ...instance,
    state: NEXT_STATE[action],
    state_reason: null,
    task_id: task.id,
    updated_at: task.created_at,
  }
  instances = instances.map((candidate) => (candidate.id === id ? updated : candidate))
  return { instance: updated }
}

/** accepted answers the 202 every lifecycle action returns, with its task. */
function accepted(instance: Instance) {
  return HttpResponse.json(instance, {
    status: 202,
    headers: { Location: `/api/v1/tasks/${instance.task_id ?? ''}` },
  })
}

/**
 * validateLaunch mirrors the launch validation in
 * `internal/service/instance/launch.go`: an RFC 1123 name free among live
 * instances, an available image, a known preset, a root disk between the image's
 * minimum and 2048, and a cloud-init payload within 16 KiB.
 */
function validateLaunch(
  body: LaunchInstanceRequest,
  availableImages: MockImage[],
  presets: InstanceType[],
): { field: string; reason: string } | null {
  if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(body.name) || body.name.length > 63) {
    return {
      field: 'name',
      reason:
        'must be 1-63 characters of lowercase letters, digits and dashes, starting and ending with a letter or digit',
    }
  }
  // A terminated row keeps its name as history, and that is what frees the name
  // for reuse, so only a live instance blocks it.
  if (
    instances.some((candidate) => candidate.name === body.name && candidate.state !== 'terminated')
  ) {
    return { field: 'name', reason: `an instance named "${body.name}" already exists` }
  }

  const image = availableImages.find((candidate) => candidate.id === body.image_id)
  if (image === undefined) {
    return { field: 'image_id', reason: `no image "${body.image_id}"` }
  }
  if (image.state !== 'available') {
    return { field: 'image_id', reason: `the image "${image.name}" is ${image.state}` }
  }
  const minDiskGb = image.min_disk_gb ?? 1

  if (body.count !== 1) {
    return {
      field: 'count',
      reason: 'must be 1; launching several instances from one image is not available yet',
    }
  }
  if (!presets.some((preset) => preset.id === body.instance_type)) {
    return { field: 'instance_type', reason: `no instance type "${body.instance_type}"` }
  }

  // The subnet is optional and defaults to the default subnet of the default
  // VPC, but a name that names one must be a real one (M4b.3).
  if (body.subnet_id !== undefined && !subnets.some((subnet) => subnet.id === body.subnet_id)) {
    return { field: 'subnet_id', reason: `no subnet "${body.subnet_id}"` }
  }

  if (body.root_disk_gb !== undefined) {
    if (body.root_disk_gb < minDiskGb) {
      return {
        field: 'root_disk_gb',
        reason: `the image "${image.name}" needs at least ${minDiskGb} GiB`,
      }
    }
    if (body.root_disk_gb > 2048) {
      return { field: 'root_disk_gb', reason: 'must be at most 2048 GiB' }
    }
  }

  if (body.user_data !== undefined && new TextEncoder().encode(body.user_data).length > 16 * 1024) {
    return { field: 'user_data', reason: 'must be at most 16 KiB' }
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
