import { expect, test, type Page } from '@playwright/test'

/**
 * The journey a new operator takes on a fresh node: create the administrator,
 * make a key pair, import an image, launch an instance, move it through its
 * lifecycle and terminate it.
 *
 * It runs against the real binary with the in-memory hypervisor, so every
 * assertion here is about the console and the backend agreeing — the thing the
 * component tests in `src/**` cannot check, because they answer their own
 * network calls.
 *
 * Serial and cumulative on purpose. Each step needs what the one before it
 * created, and the alternative (a fixture that seeds the database behind the
 * console's back) would stop testing the paths an operator actually walks.
 */

const ADMIN = 'e2e-admin'
// Twelve characters is the console's own minimum, which SetupPage enforces
// before it will submit.
const PASSWORD = 'e2e-password-1'

const KEY_PAIR = 'e2e-key'
const IMAGE = 'e2e-tiny'
const INSTANCE = 'e2e-web-01'

const fixturePort = process.env.KUMO_E2E_FIXTURE_PORT ?? '8098'
const IMAGE_URL = `http://127.0.0.1:${fixturePort}/tiny.qcow2`

/**
 * The address prefix of the suite's default VPC, from the same variable
 * playwright.config.ts hands the server. An instance's address is allocated
 * out of that subnet, so asserting on it is what proves the VPC wiring ran
 * rather than only that the row was written -- and reading it from the
 * configuration is what keeps the assertion true on a host whose own LAN
 * forced the suite onto a different range.
 */
const vpcCIDR = process.env.KUMO_E2E_VPC_CIDR ?? '10.111.0.0/24'
const ADDRESS_PREFIX = vpcCIDR.replace(/\.\d+\/\d+$/, '.')

/**
 * userMenu is the signed-in marker in the top navigation. Its accessible name
 * is the ariaLabel rather than the username, and TopNavigation renders every
 * utility twice — the visible one and the offscreen copy it measures — so the
 * first match is the one to drive.
 */
function userMenu(page: Page) {
  return page.getByRole('button', { name: 'User menu' }).first()
}

/**
 * signIn fills the login form. The access token lives in memory rather than in
 * localStorage (AGENTS.md), so a fresh browser context always starts here.
 */
async function signIn(page: Page) {
  await page.goto('/login')
  await page.getByLabel('Username').fill(ADMIN)
  await page.getByLabel('Password').fill(PASSWORD)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(userMenu(page)).toBeVisible()
}

/**
 * instanceRow is the table row of one instance, found through the link the name
 * column renders. The table selects one row at a time, so its control is a
 * radio rather than a checkbox.
 */
function instanceRow(page: Page, name: string) {
  return page.getByRole('row').filter({ has: page.getByRole('link', { name }) })
}

/**
 * chooseFromSelect drives a Cloudscape Select, which is a closed combobox: the
 * trigger carries the placeholder until something is picked, and the options
 * only exist once it is open.
 */
async function chooseFromSelect(page: Page, placeholder: RegExp, option: RegExp) {
  await page.getByRole('button', { name: placeholder }).click()
  await page.getByRole('option', { name: option }).click()
}

/**
 * instanceAction picks one lifecycle action from the dropdown on an instance's
 * page. The names are matched exactly because "Stop" is also a prefix of
 * "Force stop", and a substring match would be ambiguous between a graceful
 * ACPI request and a hard power cut.
 */
async function instanceAction(page: Page, label: string) {
  await page.getByRole('button', { name: 'Instance state' }).click()
  await page.getByRole('menuitem', { name: label, exact: true }).click()
}

test.describe.serial('the console against a real kumod', () => {
  test('a fresh node asks for an administrator and signs them in', async ({ page }) => {
    // The address an operator opens after the install, and nothing else. A
    // fresh node has to route them to first-run by itself: RequireAuth hands
    // an anonymous session to the login form, which sees that the node has no
    // users and hands off to /setup (00-architecture.md section 7).
    await page.goto('/')
    await expect(page).toHaveURL(/\/setup$/)

    await page.getByLabel('Username').fill(ADMIN)
    await page.getByLabel('Password', { exact: true }).fill(PASSWORD)
    await page.getByLabel('Confirm password').fill(PASSWORD)
    await page.getByRole('button', { name: 'Create administrator' }).click()

    // Creating the administrator signs them in, so the console opens rather
    // than bouncing back to a login form.
    await expect(userMenu(page)).toBeVisible()
    await expect(page.getByRole('link', { name: 'Instances' }).first()).toBeVisible()
  })

  test('the first-run screen is gone once an administrator exists', async ({ page }) => {
    // POST /auth/setup is refused after the first user, and the console has to
    // stop offering it or an operator is sent to a form that cannot succeed.
    await page.goto('/setup')
    await expect(page.getByRole('heading', { name: 'Sign in to Kumo' })).toBeVisible()
  })

  test('signing in with the wrong password is refused', async ({ page }) => {
    await page.goto('/login')
    await page.getByLabel('Username').fill(ADMIN)
    await page.getByLabel('Password').fill('not-the-password')
    await page.getByRole('button', { name: 'Sign in' }).click()

    await expect(page.getByText(/username or password/i)).toBeVisible()
    await expect(page).toHaveURL(/\/login$/)
  })

  test('a key pair is created and its private key shown once', async ({ page }) => {
    await signIn(page)
    await page.goto('/key-pairs/create')

    await page.getByLabel('Key pair name').fill(KEY_PAIR)
    await page.getByRole('button', { name: 'Create key pair' }).click()

    // The private key is returned exactly once, in the create response, and is
    // never persisted (AGENTS.md security rules). The dialog is the only place
    // it is ever shown.
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    await expect(dialog.getByText('This private key is shown only once')).toBeVisible()
    // By role, not by label: the modal's dismiss control is aria-labelled
    // "Save your private key before closing", which getByLabel matches as a
    // substring alongside the textarea itself.
    await expect(dialog.getByRole('textbox', { name: 'Private key' })).toHaveValue(
      /-----BEGIN OPENSSH PRIVATE KEY-----/,
    )

    // The dialog will not close until the checkbox is ticked, which is the
    // point: the key cannot be recovered after this. The checkbox is driven by
    // role rather than by its data-testid, which sits on the Cloudscape
    // wrapper and does not toggle the input underneath.
    await expect(page.getByTestId('private-key-done')).toBeDisabled()
    await dialog.getByRole('checkbox', { name: 'I have saved this key somewhere safe' }).check()
    await page.getByTestId('private-key-done').click()

    await expect(page.getByRole('link', { name: KEY_PAIR })).toBeVisible()
  })

  test('an image is imported and becomes available', async ({ page }) => {
    await signIn(page)
    await page.goto('/images/import')

    await page.getByLabel('Name').fill(IMAGE)
    await page.getByLabel('Image URL').fill(IMAGE_URL)
    // The fixture declares a 1 GiB virtual size, and the minimum is what seeds
    // the root disk on the launch form; left empty it seeds 0, which the form
    // then rejects.
    await page.getByLabel('Minimum disk size').fill('1')
    await page.getByRole('button', { name: 'Import image' }).click()

    // The import is a task: the handler answers 202 with the image in a
    // transitional state and a worker downloads it, so the console polls.
    await expect(page.getByRole('heading', { level: 1, name: IMAGE })).toBeVisible()
    await expect(page.getByText('Available')).toBeVisible({ timeout: 30_000 })
  })

  test('an instance launches from that image', async ({ page }) => {
    await signIn(page)
    await page.goto('/instances/launch')

    await page.getByLabel('Name').fill(INSTANCE)
    await chooseFromSelect(page, /Choose an image/, new RegExp(IMAGE))
    await chooseFromSelect(page, /Choose an instance type/, /kumo\.nano/)
    // Filled rather than relied on: the field is seeded from the image, and a
    // launch that silently asked for 0 GiB would fail far from this line.
    await page.getByRole('spinbutton', { name: 'Root disk' }).fill('1')

    await page.getByTestId('submit-launch').click()

    // Launching opens the instance's own page rather than returning to the
    // list, so the boot is watched where the operator was sent.
    await expect(page.getByRole('heading', { level: 1, name: INSTANCE })).toBeVisible()

    // A launch is async: the row starts pending and the worker creates the root
    // disk, the seed ISO and the domain before it reports running.
    // first(): the detail page shows the state twice, beside the heading and
    // again in the Details tab.
    await expect(page.getByText('Running').first()).toBeVisible({ timeout: 30_000 })

    // The launch allocated an address from the default subnet, which is what
    // proves the VPC wiring ran rather than just the row being written.
    await expect(page.getByText(ADDRESS_PREFIX, { exact: false }).first()).toBeVisible()
  })

  test('the instance is listed with its address and image', async ({ page }) => {
    await signIn(page)
    await page.goto('/instances')

    // The list is the page an operator lands on, so it is worth asserting that
    // the row carries what they came to read rather than only that it exists.
    const row = instanceRow(page, INSTANCE)
    await expect(row.getByText('Running')).toBeVisible()
    await expect(row).toContainText('kumo.nano')
    await expect(row).toContainText(ADDRESS_PREFIX)
    await expect(row).toContainText(IMAGE)
  })

  test('the dashboard counts the running instance and its capacity', async ({ page }) => {
    await signIn(page)
    await page.goto('/')

    // The dashboard reads the same three endpoints the API serves, so this is
    // the one place the counts, the capacity meters and a real instance meet.
    await expect(page.getByTestId('tally-total')).toHaveText('1')
    await expect(page.getByTestId('tally-running')).toHaveText('1')
    await expect(page.getByTestId('tally-error')).toHaveText('0')

    // kumo.nano is 1 vCPU, so the node has exactly one handed out.
    await expect(page.getByText(/^1 of \d+ allocated$/)).toBeVisible()

    // The launch is the newest task, and it succeeded.
    const tasks = page.getByTestId('recent-tasks')
    await expect(tasks.getByText('instance.launch')).toBeVisible()
    await expect(tasks.getByText('Succeeded').first()).toBeVisible()
  })

  test('the instance stops and starts again', async ({ page }) => {
    await signIn(page)
    await page.goto('/instances')
    await page.getByRole('link', { name: INSTANCE }).click()

    // Driven from the instance's own page rather than the list. The actions are
    // the same, but the list applies them to whatever row is selected, and the
    // poll that brings a new state in re-renders the table and drops that
    // selection — so a test that clicked through the list would be asserting
    // against the selection model as much as against the lifecycle.
    await expect(page.getByRole('heading', { level: 1, name: INSTANCE })).toBeVisible()

    await instanceAction(page, 'Stop')
    await expect(page.getByText('Stopped').first()).toBeVisible({ timeout: 30_000 })

    await instanceAction(page, 'Start')
    await expect(page.getByText('Running').first()).toBeVisible({ timeout: 30_000 })
  })

  test('terminating the instance removes it', async ({ page }) => {
    await signIn(page)
    await page.goto('/instances')
    await page.getByRole('link', { name: INSTANCE }).click()
    await expect(page.getByRole('heading', { level: 1, name: INSTANCE })).toBeVisible()

    await instanceAction(page, 'Terminate')

    // A terminate is destructive, so the modal asks for the name back before it
    // will enable its button (AGENTS.md frontend conventions).
    const dialog = page.getByRole('dialog')
    await expect(dialog).toBeVisible()
    await expect(page.getByTestId('confirm-destructive')).toBeDisabled()
    await dialog.getByRole('textbox', { name: 'Type the name to confirm' }).fill(INSTANCE)
    await page.getByTestId('confirm-destructive').click()

    // A terminate deletes the row rather than keeping it as history
    // (02-data-model.md), so the instance leaves the table altogether.
    await page.goto('/instances')
    await expect(page.getByRole('link', { name: INSTANCE })).toBeHidden({ timeout: 30_000 })
  })

  test('the image is deleted once nothing uses it', async ({ page }) => {
    await signIn(page)
    await page.goto('/images')

    // The delete was refused with a 409 while the instance existed, because an
    // image in use cannot go; with the instance terminated the image is free.
    const row = page.getByRole('row').filter({ has: page.getByRole('link', { name: IMAGE }) })
    await expect(row).toBeVisible()

    // Multi-select here, unlike the instances table, so the control is a
    // checkbox.
    await row.getByRole('checkbox').check()
    await page.getByTestId('delete-images').click()

    const dialog = page.getByRole('dialog')
    await expect(dialog.getByText('This cannot be undone')).toBeVisible()
    await page.getByTestId('confirm-delete').click()

    await expect(page.getByRole('link', { name: IMAGE })).toBeHidden({ timeout: 30_000 })
  })

  test('signing out ends the session', async ({ page }) => {
    await signIn(page)

    await userMenu(page).click()
    await page.getByRole('menuitem', { name: 'Sign out' }).click()

    await expect(page.getByRole('heading', { name: 'Sign in to Kumo' })).toBeVisible()

    // The refresh cookie is gone too, so a reload cannot restore the session.
    await page.goto('/instances')
    await expect(page).toHaveURL(/\/login/)
  })
})
