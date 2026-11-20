import { readFile } from 'node:fs/promises'
import { expect, test, type APIRequestContext, type Page } from '@playwright/test'

/**
 * The journey an operator takes through the Kubernetes pages: import the
 * image, create a cluster, download a kubeconfig, resize the node group and
 * delete the cluster (08-kubernetes.md, M8.8).
 *
 * Against the real binary on the in-memory hypervisor, like the other specs:
 * kumod's cluster API is the fake one there, which answers as a healthy
 * cluster would, so every assertion is about the console and the backend
 * agreeing. What a real cluster does is `make test-k8s-journey`'s.
 *
 * Serial and cumulative: each step needs what the one before it created.
 */

const ADMIN = 'e2e-admin'
const PASSWORD = 'e2e-password-1'
const IMAGE = 'e2e-kubernetes-1.37'
const CLUSTER = 'e2e-shop'

const fixturePort = process.env.KUMO_E2E_FIXTURE_PORT ?? '8098'
const IMAGE_URL = `http://127.0.0.1:${fixturePort}/tiny.qcow2`

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

async function apiToken(request: APIRequestContext): Promise<string> {
  const response = await request.post('/api/v1/auth/login', {
    data: { username: ADMIN, password: PASSWORD },
  })
  expect(response.ok()).toBeTruthy()
  const body = (await response.json()) as { access_token: string }
  return body.access_token
}

async function chooseFromSelect(page: Page, trigger: RegExp, option: RegExp) {
  await page.getByRole('button', { name: trigger }).click()
  await page.getByRole('option', { name: option }).click()
}

async function openCluster(page: Page) {
  await page.goto('/kubernetes')
  await page.getByRole('link', { name: CLUSTER }).click()
  await expect(page.getByRole('heading', { level: 1, name: CLUSTER })).toBeVisible()
}

test.describe.serial('the Kubernetes console against a real kumod', () => {
  test('a fresh node has an administrator', async ({ page, request }) => {
    // A second setup is refused, which is the state this test wants either way.
    await request.post('/api/v1/auth/setup', {
      data: { username: ADMIN, password: PASSWORD },
    })
    await signIn(page)
  })

  test('the Kubernetes pages explain the image before there is one', async ({ page }) => {
    await signIn(page)
    await page.goto('/kubernetes')

    await expect(page.getByRole('heading', { level: 1, name: 'Clusters' })).toBeVisible()
    await expect(page.getByText('No Kubernetes image is available yet')).toBeVisible()
    await expect(page.getByTestId('create-cluster')).toBeDisabled()

    // The module is on by default: the section and the Services entry are there.
    await expect(page.getByRole('link', { name: 'Clusters' }).first()).toBeVisible()
    await page.getByRole('button', { name: 'Services' }).first().click()
    await expect(page.getByRole('menuitem', { name: /Kubernetes/ })).toBeVisible()
  })

  test('the image is imported through the API', async ({ page, request }) => {
    const token = await apiToken(request)
    const imported = await request.post('/api/v1/images/import', {
      headers: { authorization: `Bearer ${token}` },
      data: {
        name: IMAGE,
        source_url: IMAGE_URL,
        os_family: 'linux',
        architecture: 'x86_64',
        default_user: 'ubuntu',
        min_disk_gb: 1,
        kind: 'engine',
        engine: 'kubernetes',
        engine_version: '1.37',
      },
    })
    expect(imported.status()).toBe(202)

    await signIn(page)
    await page.goto('/kubernetes')
    await expect(page.getByText('No Kubernetes image is available yet')).toBeHidden({
      timeout: 30_000,
    })
    await expect(page.getByTestId('create-cluster')).toBeEnabled()
  })

  test('a cluster is created and becomes active', async ({ page }) => {
    await signIn(page)
    await page.goto('/kubernetes/create')

    await page.getByLabel('Cluster name').fill(CLUSTER)
    await expect(page.getByRole('button', { name: /Kubernetes version/ })).toContainText('1.37')
    // The control plane needs 2 vCPU; the workers fit the smallest 2 GiB type,
    // which keeps four nodes inside the fake node's memory.
    await chooseFromSelect(page, /Worker size/, /^kumo\.small/)
    await page.getByLabel('Workers').fill('2')
    await expect(page.getByText('Reachable from inside its VPC')).toBeVisible()

    const created = page.waitForResponse(
      (r) => r.request().method() === 'POST' && r.url().endsWith('/api/v1/eks/clusters'),
    )
    await page.getByTestId('submit-create-cluster').click()
    const response = await created
    expect(response.status()).toBe(202)
    expect(await response.json()).toMatchObject({ state: 'creating', task_id: expect.any(String) })

    await expect(page.getByRole('heading', { level: 1, name: CLUSTER })).toBeVisible()
    await expect(page.getByText('Active').first()).toBeVisible({ timeout: 60_000 })

    await page.getByRole('tab', { name: 'Nodes (3)' }).click()
    const nodes = page.getByRole('table', { name: 'Nodes' })
    await expect(nodes.getByText(`${CLUSTER}-cp-1`)).toBeVisible()
    await expect(nodes.getByText(`${CLUSTER}-default-2`)).toBeVisible()
  })

  test('the managed nodes are not on the Instances pages', async ({ page, request }) => {
    // The API leaves them out, which is what every Instances page reads.
    const token = await apiToken(request)
    const listed = await request.get('/api/v1/instances?page_size=200', {
      headers: { authorization: `Bearer ${token}` },
    })
    expect(listed.ok()).toBeTruthy()
    expect(await listed.text()).not.toContain(CLUSTER)

    await signIn(page)
    await page.goto('/instances')
    await expect(page.getByRole('table').first()).toBeVisible()
    await expect(page.getByText(`${CLUSTER}-cp-1`)).toBeHidden()
  })

  test('Connect downloads an administrator kubeconfig', async ({ page }) => {
    await signIn(page)
    await openCluster(page)
    await page.getByRole('tab', { name: 'Connect' }).click()

    await expect(page.getByText('kumo:admins')).toBeVisible()
    await expect(page.getByText(`api.${CLUSTER}.default`).first()).toBeVisible()
    await expect(page.getByText('Pods do not get NET_RAW')).toBeVisible()

    const minted = page.waitForResponse(
      (r) => r.request().method() === 'POST' && r.url().endsWith('/kubeconfig'),
    )
    const download = page.waitForEvent('download')
    await page.getByTestId('download-kubeconfig').click()

    // Never cached anywhere on the way: the file carries a private key.
    const response = await minted
    expect(response.headers()['cache-control']).toContain('no-store')

    const file = await download
    expect(file.suggestedFilename()).toBe(`${CLUSTER}.kubeconfig`)
    const path = await file.path()
    const kubeconfig = await readFile(path, 'utf8')
    expect(kubeconfig).toContain('kind: Config')
    expect(kubeconfig).toContain(`https://api.${CLUSTER}.default:6443`)
    expect(kubeconfig).toContain('client-key-data')
    await expect(page.getByText(/Downloaded; valid until/)).toBeVisible()
  })

  test('the node group is resized up and down', async ({ page }) => {
    await signIn(page)
    await openCluster(page)

    for (const size of ['3', '1']) {
      await page.getByTestId('resize-node-group').click()
      const dialog = page.getByRole('dialog')
      await dialog.getByLabel('Nodes').fill(size)
      if (size === '1') {
        await expect(dialog.getByText('The newest workers are drained first')).toBeVisible()
      }
      await page.getByTestId('confirm-resize').click()
      await expect(dialog).toBeHidden()
      await expect(page.getByText('Active').first()).toBeVisible({ timeout: 60_000 })
      const total = Number(size) + 1
      await expect(page.getByRole('tab', { name: `Nodes (${total})` })).toBeVisible({
        timeout: 60_000,
      })
    }
  })

  test('deleting the cluster takes its nodes with it', async ({ page }) => {
    await signIn(page)
    await openCluster(page)

    await page.getByTestId('delete-cluster').click()
    const dialog = page.getByRole('dialog')
    await expect(dialog.getByText('Everything on the cluster is deleted with it')).toBeVisible()
    await expect(page.getByTestId('confirm-delete-cluster')).toBeDisabled()
    await dialog.getByRole('textbox', { name: 'Cluster name to confirm' }).fill(CLUSTER)
    await page.getByTestId('confirm-delete-cluster').click()

    await page.goto('/kubernetes')
    await expect(page.getByRole('link', { name: CLUSTER })).toBeHidden({ timeout: 60_000 })
  })
})
