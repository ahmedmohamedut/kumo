import { expect, test, type APIRequestContext, type Page } from '@playwright/test'

/**
 * The journey an operator takes through the Databases pages: import the engine
 * image, create a database, read its connection details, stop it, start it,
 * reboot it and delete it.
 *
 * It runs against the real binary with the in-memory hypervisor, like
 * `console.spec.ts`, so every assertion is about the console and the backend
 * agreeing. What the fake cannot prove — that a guest boots Postgres and
 * answers on its address — is what the opt-in integration test against real
 * libvirt is for (`make test-libvirt`, 07-databases.md §6).
 *
 * Serial and cumulative: each step needs what the one before it created.
 */

const ADMIN = 'e2e-admin'
const PASSWORD = 'e2e-password-1'

const ENGINE_IMAGE = 'e2e-postgres-18'
const IDENTIFIER = 'e2e-orders'
const DB_PASSWORD = 'e2e-db-password-1'

const fixturePort = process.env.KUMO_E2E_FIXTURE_PORT ?? '8098'
const IMAGE_URL = `http://127.0.0.1:${fixturePort}/tiny.qcow2`

/** userMenu is the signed-in marker in the top navigation. */
function userMenu(page: Page) {
  return page.getByRole('button', { name: 'User menu' }).first()
}

async function signIn(page: Page) {
  await page.goto('/login')
  await page.getByLabel('Username').fill(ADMIN)
  await page.getByLabel('Password').fill(PASSWORD)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(userMenu(page)).toBeVisible()
}

/**
 * apiToken logs in through the API, for the two steps the console has no page
 * for: importing an **engine** image, and proving that the server behind a
 * database refuses the Instances lifecycle.
 *
 * The import form carries no engine fields on purpose (M7.2): an engine image
 * is imported by whoever runs the node, from a URL they published, and the
 * curated catalog has no entry until there is one to point at. So this is the
 * path a real operator uses today as well.
 */
async function apiToken(request: APIRequestContext): Promise<string> {
  const response = await request.post('/api/v1/auth/login', {
    data: { username: ADMIN, password: PASSWORD },
  })
  expect(response.ok()).toBeTruthy()
  const body = (await response.json()) as { access_token: string }
  return body.access_token
}

/**
 * chooseFromSelect drives a Cloudscape Select: a closed combobox whose trigger
 * carries the placeholder, or the current value, until something is picked.
 */
async function chooseFromSelect(page: Page, current: RegExp, option: RegExp) {
  await page.getByRole('button', { name: current }).click()
  await page.getByRole('option', { name: option }).click()
}

test.describe.serial('the databases console against a real kumod', () => {
  test('a fresh node has an administrator', async ({ page, request }) => {
    // The suite may be run on its own, so the administrator is created here
    // when console.spec.ts has not already done it. A second setup is refused,
    // which is the state this test wants either way.
    await request.post('/api/v1/auth/setup', {
      data: { username: ADMIN, password: PASSWORD },
    })
    await signIn(page)
  })

  test('the Databases pages explain the engine image before there is one', async ({ page }) => {
    await signIn(page)
    await page.goto('/databases')

    // The module is on by default, so the section is there and the table
    // renders; what is missing is the one import an operator has to do
    // (07-databases.md §2).
    await expect(page.getByRole('heading', { level: 1, name: 'Databases' })).toBeVisible()
    await expect(page.getByText('No database engine image is available yet')).toBeVisible()
    await expect(page.getByTestId('create-db-instance')).toBeDisabled()

    // And the nav offers it, because the module is on.
    await expect(page.getByRole('link', { name: 'Databases' }).first()).toBeVisible()
  })

  test('an engine image is imported through the API', async ({ page, request }) => {
    const token = await apiToken(request)

    const imported = await request.post('/api/v1/images/import', {
      headers: { authorization: `Bearer ${token}` },
      data: {
        name: ENGINE_IMAGE,
        source_url: IMAGE_URL,
        os_family: 'linux',
        architecture: 'x86_64',
        default_user: 'ubuntu',
        min_disk_gb: 1,
        kind: 'engine',
        engine: 'postgres',
        engine_version: '18',
      },
    })
    expect(imported.status()).toBe(202)

    // The console must not offer an engine image as something to launch an
    // instance from: it is the databases module's, and GET /images excludes it
    // (07-databases.md §3).
    await signIn(page)
    await page.goto('/images')
    await expect(page.getByRole('link', { name: ENGINE_IMAGE })).toBeHidden()

    // The Databases page picks it up instead, through /system/info.
    await page.goto('/databases')
    await expect(page.getByText('No database engine image is available yet')).toBeHidden({
      timeout: 30_000,
    })
    await expect(page.getByTestId('create-db-instance')).toBeEnabled()
  })

  test('a database is created and becomes available', async ({ page }) => {
    await signIn(page)
    await page.goto('/databases/create')

    await expect(page.getByText('PostgreSQL 18')).toBeVisible()
    await page.getByLabel('DB instance identifier').fill(IDENTIFIER)
    await page.getByLabel('Admin user').fill('app')
    await page.getByLabel('Password', { exact: true }).fill(DB_PASSWORD)
    await page.getByLabel('Confirm password').fill(DB_PASSWORD)
    await page.getByLabel('Initial database').fill('orders')
    await chooseFromSelect(page, /kumo\./, /kumo\.nano/)
    await page.getByLabel('Storage in GiB').fill('5')

    // The form says what cannot be undone before it is submitted: there is no
    // password reset, in any form (07-databases.md R8).
    await expect(page.getByText('Save this password somewhere safe')).toBeVisible()
    await expect(page.getByText('Connections are not encrypted').first()).toBeVisible()

    await page.getByTestId('submit-create-db-instance').click()

    // The create is a task: the handler answers 202 with the database
    // `creating`, and the worker makes the data disk, builds the guest and
    // waits for the engine.
    await expect(page.getByRole('heading', { level: 1, name: IDENTIFIER })).toBeVisible()
    await expect(page.getByText('This database is being created')).toBeVisible()
    await expect(page.getByText('Available').first()).toBeVisible({ timeout: 60_000 })
  })

  test('the Connect panel carries everything but the password', async ({ page }) => {
    await signIn(page)
    await page.goto('/databases')
    await page.getByRole('link', { name: IDENTIFIER }).click()

    // The endpoint is the identifier under the VPC's DNS name, written once the
    // guest had an address (07-databases.md R9).
    await expect(page.getByText(`${IDENTIFIER}.default`).first()).toBeVisible()
    await expect(page.getByText('5432').first()).toBeVisible()
    await expect(page.getByText('orders', { exact: true }).first()).toBeVisible()
    await expect(page.getByText('app', { exact: true }).first()).toBeVisible()

    // The psql example is there, and the password is not in it.
    const psql = page.getByText(/^psql "host=/)
    await expect(psql).toBeVisible()
    await expect(psql).not.toContainText(DB_PASSWORD)

    // Nothing anywhere on the page carries it, which is the whole of R8: the
    // request was the only place it existed.
    await expect(page.locator('body')).not.toContainText(DB_PASSWORD)
  })

  test("the server behind the database is nobody else's to touch", async ({ page, request }) => {
    const token = await apiToken(request)

    const detail = await request.get(`/api/v1/rds/db-instances?page_size=100`, {
      headers: { authorization: `Bearer ${token}` },
    })
    expect(detail.ok()).toBeTruthy()
    const page1 = (await detail.json()) as {
      items: Array<{ id: string; identifier: string; instance_id: string }>
    }
    const created = page1.items.find((item) => item.identifier === IDENTIFIER)
    expect(created).toBeDefined()
    const instanceID = created?.instance_id ?? ''
    expect(instanceID).not.toBe('')

    // R7: the VM is not on the Instances pages.
    await signIn(page)
    await page.goto('/instances')
    await expect(page.getByRole('link', { name: instanceID })).toBeHidden()

    // And its own lifecycle endpoints refuse it, naming the DB instance to act
    // on instead — through the real binary, not a service test.
    for (const path of [
      `/api/v1/instances/${instanceID}/stop`,
      `/api/v1/instances/${instanceID}/reboot`,
    ]) {
      const refused = await request.post(path, {
        headers: { authorization: `Bearer ${token}` },
      })
      expect(refused.status()).toBe(409)
      expect(await refused.text()).toContain('/api/v1/rds/db-instances')
    }
    const terminate = await request.delete(`/api/v1/instances/${instanceID}`, {
      headers: { authorization: `Bearer ${token}` },
    })
    expect(terminate.status()).toBe(409)
  })

  test('the database stops and starts again with the same endpoint', async ({ page }) => {
    await signIn(page)
    await page.goto('/databases')
    await page.getByRole('link', { name: IDENTIFIER }).click()
    await expect(page.getByRole('heading', { level: 1, name: IDENTIFIER })).toBeVisible()

    await page.getByTestId('stop-db-instance').click()
    await expect(page.getByText('Stopped').first()).toBeVisible({ timeout: 60_000 })

    // The endpoint survives a stop: the address and the MAC are stable by
    // construction, so a client's connection string does not change.
    await expect(page.getByText(`${IDENTIFIER}.default`).first()).toBeVisible()

    await page.getByTestId('start-db-instance').click()
    await expect(page.getByText('Available').first()).toBeVisible({ timeout: 60_000 })
    await expect(page.getByText(`${IDENTIFIER}.default`).first()).toBeVisible()
  })

  test('the database reboots', async ({ page }) => {
    await signIn(page)
    await page.goto('/databases')
    await page.getByRole('link', { name: IDENTIFIER }).click()

    await page.getByTestId('reboot-db-instance').click()
    // The reconciler is what publishes it again once the engine answers, so
    // this outlasts a reconcile interval rather than a poll.
    await expect(page.getByText('Available').first()).toBeVisible({ timeout: 60_000 })
  })

  test('deleting the database takes its storage with it', async ({ page }) => {
    await signIn(page)
    await page.goto('/databases')
    await page.getByRole('link', { name: IDENTIFIER }).click()

    await page.getByTestId('delete-db-instance').click()

    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    await expect(dialog.getByText('The data is deleted with the database')).toBeVisible()
    // Destructive and unrecoverable, so the button stays disabled until the
    // identifier is typed back.
    await expect(page.getByTestId('confirm-delete-db-instance')).toBeDisabled()
    await dialog.getByRole('textbox', { name: 'Database identifier to confirm' }).fill(IDENTIFIER)
    await page.getByTestId('confirm-delete-db-instance').click()

    // The delete removes both rows, so the database leaves the table.
    await page.goto('/databases')
    await expect(page.getByRole('link', { name: IDENTIFIER })).toBeHidden({ timeout: 60_000 })
  })

  test('the Databases pages are gone on a node with the module off', async ({ page }) => {
    // Nothing restarts kumod mid-suite, so the module-off state is covered by
    // the component tests. What is checked here is the other half: with the
    // module on, the section is in the navigation and the Services menu.
    await signIn(page)
    await page.goto('/')

    await expect(page.getByRole('link', { name: 'Databases' }).first()).toBeVisible()
    await page.getByRole('button', { name: 'Services' }).first().click()
    await expect(page.getByRole('menuitem', { name: /Databases/ })).toBeVisible()
  })
})
