import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { CreateDBInstancePage } from '@/features/dbinstances/CreateDBInstancePage'
import { DBInstanceDetailPage } from '@/features/dbinstances/DBInstanceDetailPage'
import { DBInstancesListPage } from '@/features/dbinstances/DBInstancesListPage'
import {
  identifierProblem,
  passwordProblem,
  pgNameProblem,
  psqlExample,
  storageProblem,
} from '@/features/dbinstances/state'
import {
  MOCK_ENGINES,
  MOCK_VIEWER,
  mockDBInstances,
  setMockDatabases,
  setMockDBInstances,
} from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

function Pages() {
  return (
    <Routes>
      <Route path="/databases" element={<DBInstancesListPage />} />
      <Route path="/databases/create" element={<CreateDBInstancePage />} />
      <Route path="/databases/:dbInstanceId" element={<DBInstanceDetailPage />} />
    </Routes>
  )
}

function rowOf(identifier: string): HTMLElement {
  const row = screen.getByRole('link', { name: identifier }).closest('tr')
  if (row === null) {
    throw new Error(`no table row for ${identifier}`)
  }
  return row
}

async function select(identifier: string) {
  await userEvent.click(within(rowOf(identifier)).getByRole('radio'))
}

/**
 * actionsButton is the lifecycle dropdown trigger. ButtonDropdown puts its
 * `data-testid` on the wrapper rather than the inner button, so the trigger is
 * queried by its accessible name.
 */
function actionsButton(): HTMLElement {
  return screen.getByRole('button', { name: 'Actions' })
}

async function openActions() {
  await userEvent.click(actionsButton())
}

/**
 * menuItem is one entry of the opened dropdown. A disabled entry is a
 * `<span role="menuitem" aria-disabled="true">` rather than a disabled button,
 * so the assertion is on `aria-disabled` -- which is what stops the click and
 * what a screen reader announces.
 */
async function menuItem(label: string) {
  return screen.findByRole('menuitem', { name: label })
}

function itemEnabled(item: HTMLElement): boolean {
  return item.getAttribute('aria-disabled') !== 'true'
}

function dbInstanceOf(identifier: string) {
  return mockDBInstances().find((candidate) => candidate.identifier === identifier)
}

describe('DBInstancesListPage', () => {
  it('lists the databases with their state and endpoint', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases' })

    expect(await screen.findByRole('link', { name: 'orders' })).toBeInTheDocument()
    const orders = rowOf('orders')
    expect(within(orders).getByText('Available')).toBeInTheDocument()
    expect(within(orders).getByText('Postgres 18')).toBeInTheDocument()
    expect(within(orders).getByText('orders.default')).toBeInTheDocument()
    expect(within(orders).getByText('20 GiB')).toBeInTheDocument()

    // The state is the database's, not the server's: an engine that stopped
    // answering reads as Failed even though its guest is running.
    expect(within(rowOf('reports')).getByText('Failed')).toBeInTheDocument()
  })

  it('explains how to turn the module on when it is off', async () => {
    setMockDatabases({ enabled: false, engines: [] })
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases' })

    expect(await screen.findByText('Databases are turned off on this node')).toBeInTheDocument()
    expect(screen.getByText('KUMO_DATABASES=on')).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('explains the engine image import when there is none', async () => {
    setMockDatabases({ enabled: true, engines: [] })
    setMockDBInstances([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases' })

    expect(await screen.findByText('No database engine image is available yet')).toBeInTheDocument()
    expect(screen.getByText('packaging/images/postgres-18/')).toBeInTheDocument()
    // The module is on, so the table is still there; what cannot be done yet
    // is a create.
    expect(screen.getByRole('table')).toBeInTheDocument()
    expect(screen.getByTestId('create-db-instance')).toBeDisabled()
  })

  it('keeps the write actions away from a viewer', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/databases' })

    await screen.findByRole('link', { name: 'orders' })
    expect(screen.getByTestId('create-db-instance')).toBeDisabled()
    expect(actionsButton()).toBeDisabled()
    expect(screen.queryByRole('radio')).not.toBeInTheDocument()
  })

  it('offers only the actions the state allows', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases' })

    await screen.findByRole('link', { name: 'orders' })
    await select('orders')
    await openActions()

    // Available: stop, reboot and delete apply, start does not.
    expect(itemEnabled(await menuItem('Stop'))).toBe(true)
    expect(itemEnabled(await menuItem('Reboot'))).toBe(true)
    expect(itemEnabled(await menuItem('Delete'))).toBe(true)
    expect(itemEnabled(await menuItem('Start'))).toBe(false)
  })

  it('offers start, and not stop, for a database that is stopped', async () => {
    setMockDBInstances(
      mockDBInstances().map((candidate) =>
        candidate.identifier === 'orders' ? { ...candidate, state: 'stopped' } : candidate,
      ),
    )
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases' })

    await screen.findByRole('link', { name: 'orders' })
    await select('orders')
    await openActions()

    expect(itemEnabled(await menuItem('Start'))).toBe(true)
    expect(itemEnabled(await menuItem('Stop'))).toBe(false)
    expect(itemEnabled(await menuItem('Reboot'))).toBe(false)
  })

  it('stops a database', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/databases' })

    await screen.findByRole('link', { name: 'orders' })
    await select('orders')
    await openActions()
    await userEvent.click(await menuItem('Stop'))

    await waitFor(() => {
      expect(dbInstanceOf('orders')?.state).toBe('stopping')
    })
    // The Flashbar lives in the layout, so the notification is asserted on the
    // store the page dispatched it to.
    await waitFor(() => {
      expect(store.getState().notifications.items[0]?.header).toBe('Database stopping')
    })
  })

  it('reports a refused action rather than looking like it worked', async () => {
    // Only `orders`, and available: nothing is transitional, so the list does
    // not refetch and the page keeps the row it rendered.
    const orders = mockDBInstances().filter((candidate) => candidate.identifier === 'orders')
    setMockDBInstances(orders)
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/databases' })

    await screen.findByRole('link', { name: 'orders' })
    await select('orders')

    // The database is stopped out of band: the page still offers Stop, and the
    // API refuses it with a 409. That refusal has to reach the operator rather
    // than leaving a button that looks like it worked.
    setMockDBInstances(orders.map((candidate) => ({ ...candidate, state: 'stopped' })))

    await openActions()
    await userEvent.click(await menuItem('Stop'))

    await waitFor(() => {
      const error = store.getState().notifications.items.find((item) => item.type === 'error')
      expect(error?.header).toBe('The database could not be stopped')
    })
  })

  it('deletes a database once the identifier is typed', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases' })

    await screen.findByRole('link', { name: 'orders' })
    await select('orders')
    await openActions()
    await userEvent.click(await menuItem('Delete'))

    // The data is gone with the database, so the dialog says so and the button
    // stays disabled until the identifier is typed exactly.
    expect(await screen.findByText('The data is deleted with the database')).toBeInTheDocument()
    const confirm = screen.getByTestId('confirm-delete-db-instance')
    expect(confirm).toBeDisabled()

    await userEvent.type(screen.getByLabelText('Database identifier to confirm'), 'order')
    expect(confirm).toBeDisabled()
    await userEvent.type(screen.getByLabelText('Database identifier to confirm'), 's')
    await waitFor(() => {
      expect(confirm).toBeEnabled()
    })

    await userEvent.click(confirm)
    await waitFor(() => {
      expect(dbInstanceOf('orders')?.state).toBe('deleting')
    })
  })

  it('shows an empty state on a node with no databases', async () => {
    setMockDBInstances([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases' })

    expect(await screen.findByText('No databases')).toBeInTheDocument()
  })
})

describe('CreateDBInstancePage', () => {
  async function fill(values: {
    identifier?: string
    adminUsername?: string
    password?: string
    confirm?: string
    databaseName?: string
  }) {
    if (values.identifier !== undefined) {
      await userEvent.type(screen.getByLabelText('DB instance identifier'), values.identifier)
    }
    if (values.adminUsername !== undefined) {
      await userEvent.type(screen.getByLabelText('Admin user'), values.adminUsername)
    }
    if (values.password !== undefined) {
      await userEvent.type(screen.getByLabelText('Password'), values.password)
    }
    if (values.confirm !== undefined) {
      await userEvent.type(screen.getByLabelText('Confirm password'), values.confirm)
    }
    if (values.databaseName !== undefined) {
      await userEvent.type(screen.getByLabelText('Initial database'), values.databaseName)
    }
  }

  it('says the password cannot be recovered or reset', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/create' })

    expect(await screen.findByText('Save this password somewhere safe')).toBeInTheDocument()
    expect(
      screen.getByText(/There is no password reset in this release/, { exact: false }),
    ).toBeInTheDocument()
    // And the field itself says why, beside it.
    expect(
      screen.getByText(/Kumo does not keep this password/, { exact: false }),
    ).toBeInTheDocument()
  })

  it('warns that connections are not encrypted', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/create' })

    expect(await screen.findByText('Connections are not encrypted')).toBeInTheDocument()
  })

  it('creates a database and opens its page', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/databases/create' })

    await screen.findByLabelText('DB instance identifier')
    await fill({
      identifier: 'billing',
      adminUsername: 'app',
      password: 'a long enough password',
      confirm: 'a long enough password',
      databaseName: 'billing',
    })
    await userEvent.click(screen.getByTestId('submit-create-db-instance'))

    await waitFor(() => {
      expect(dbInstanceOf('billing')?.state).toBe('creating')
    })
    await waitFor(() => {
      expect(store.getState().notifications.items[0]?.header).toBe('Database creation started')
    })
    // The detail page of the new database, which explains what is happening.
    expect(await screen.findByText('This database is being created')).toBeInTheDocument()
  })

  it('refuses a bad identifier before the request', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/create' })

    await screen.findByLabelText('DB instance identifier')
    await fill({
      identifier: 'Orders',
      adminUsername: 'app',
      password: 'a long enough password',
      confirm: 'a long enough password',
      databaseName: 'orders',
    })
    await userEvent.click(screen.getByTestId('submit-create-db-instance'))

    expect(
      await screen.findByText(/Must start with a letter/, { exact: false }),
    ).toBeInTheDocument()
    expect(dbInstanceOf('Orders')).toBeUndefined()
  })

  it('refuses a reserved admin user and a short password locally', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/create' })

    await screen.findByLabelText('DB instance identifier')
    await fill({
      identifier: 'billing',
      adminUsername: 'postgres',
      password: 'short',
      confirm: 'short',
      databaseName: 'billing',
    })
    await userEvent.click(screen.getByTestId('submit-create-db-instance'))

    expect(await screen.findByText(/is reserved by Postgres/, { exact: false })).toBeInTheDocument()
    expect(screen.getByText('Must be at least 10 characters.')).toBeInTheDocument()
    expect(dbInstanceOf('billing')).toBeUndefined()
  })

  it('catches a password that is not printable ASCII, which would not work', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/create' })

    await screen.findByLabelText('DB instance identifier')
    await fill({
      identifier: 'billing',
      adminUsername: 'app',
      password: 'passphrase—long enough',
      confirm: 'passphrase—long enough',
      databaseName: 'billing',
    })
    await userEvent.click(screen.getByTestId('submit-create-db-instance'))

    expect(
      await screen.findByText(/Must hold printable ASCII only/, { exact: false }),
    ).toBeInTheDocument()
    expect(dbInstanceOf('billing')).toBeUndefined()
  })

  it('reports an identifier the API has already', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/create' })

    await screen.findByLabelText('DB instance identifier')
    await fill({
      identifier: 'orders',
      adminUsername: 'app',
      password: 'a long enough password',
      confirm: 'a long enough password',
      databaseName: 'orders',
    })
    await userEvent.click(screen.getByTestId('submit-create-db-instance'))

    expect(await screen.findByText('The database was not created')).toBeInTheDocument()
    expect(
      screen.getByText(/already exists/, { exact: false, selector: 'div' }),
    ).toBeInTheDocument()
  })

  it('refuses two passwords that do not match', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/create' })

    await screen.findByLabelText('DB instance identifier')
    await fill({
      identifier: 'billing',
      adminUsername: 'app',
      password: 'a long enough password',
      confirm: 'a different password',
      databaseName: 'billing',
    })
    await userEvent.click(screen.getByTestId('submit-create-db-instance'))

    expect(await screen.findByText('The two passwords do not match.')).toBeInTheDocument()
    expect(dbInstanceOf('billing')).toBeUndefined()
  })

  it('explains the engine image import instead of a form when there is none', async () => {
    setMockDatabases({ enabled: true, engines: [] })
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/create' })

    expect(await screen.findByText('No database engine image is available yet')).toBeInTheDocument()
    expect(screen.queryByLabelText('DB instance identifier')).not.toBeInTheDocument()
  })

  it('tells a viewer the create is not theirs', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/databases/create' })

    expect(
      await screen.findByText('Only an administrator can create databases.'),
    ).toBeInTheDocument()
    expect(screen.getByTestId('submit-create-db-instance')).toBeDisabled()
  })

  it('offers the engines the node reports', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/create' })

    expect(
      await screen.findByText(`PostgreSQL ${String(MOCK_ENGINES[0]?.version)}`),
    ).toBeInTheDocument()
  })
})

describe('DBInstanceDetailPage', () => {
  it('shows the Connect panel without a password', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/db-0a1b2c3d4e5f60718' })

    expect(await screen.findByRole('heading', { name: 'orders' })).toBeInTheDocument()
    expect(screen.getByText('Connect')).toBeInTheDocument()
    expect(screen.getByText('orders.default')).toBeInTheDocument()
    expect(screen.getByText('5432')).toBeInTheDocument()
    expect(screen.getByText('app')).toBeInTheDocument()

    const expected = psqlExample({
      ...mockDBInstances()[0]!,
    })
    expect(screen.getByText(expected)).toBeInTheDocument()
    // The password is not in the example and cannot be: psql prompts for it.
    expect(expected).not.toContain('password')
    expect(
      screen.getByText(/Kumo derived the engine's credential from it and discarded it/, {
        exact: false,
      }),
    ).toBeInTheDocument()
  })

  it('warns that the connection is not encrypted', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/db-0a1b2c3d4e5f60718' })

    expect(await screen.findByText('Connections are not encrypted')).toBeInTheDocument()
    expect(screen.getByText('sslmode=require')).toBeInTheDocument()
  })

  it('names the server behind the database so its log can be found', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/db-0a1b2c3d4e5f60718' })

    expect(await screen.findByText('i-0a1b2c3d4e5f60718')).toBeInTheDocument()
  })

  it('explains a failed database and what to do about it', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/db-0b2c3d4e5f6071890' })

    expect(await screen.findByText('This database is not answering')).toBeInTheDocument()
    expect(
      screen.getByText('engine_not_responding: nothing answers on 10.0.0.9:5432'),
    ).toBeInTheDocument()
    // A failed database can be rebooted or deleted, not stopped or started.
    expect(screen.getByTestId('delete-db-instance')).toBeEnabled()
    expect(screen.getByTestId('stop-db-instance')).toBeDisabled()
    expect(screen.getByTestId('start-db-instance')).toBeDisabled()
  })

  it('stops a database from its page', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/db-0a1b2c3d4e5f60718' })

    await screen.findByRole('heading', { name: 'orders' })
    await userEvent.click(screen.getByTestId('stop-db-instance'))

    await waitFor(() => {
      expect(dbInstanceOf('orders')?.state).toBe('stopping')
    })
  })

  it('keeps the actions away from a viewer but shows the connection details', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/databases/db-0a1b2c3d4e5f60718' })

    await screen.findByRole('heading', { name: 'orders' })
    expect(screen.getByText('orders.default')).toBeInTheDocument()
    for (const action of [
      'stop-db-instance',
      'start-db-instance',
      'reboot-db-instance',
      'delete-db-instance',
    ]) {
      expect(screen.getByTestId(action)).toBeDisabled()
    }
  })

  it('says so when the database is gone', async () => {
    setMockDBInstances([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/db-0a1b2c3d4e5f60718' })

    expect(await screen.findByText('This database is gone')).toBeInTheDocument()
  })

  it('explains how to turn the module on when it is off', async () => {
    setMockDatabases({ enabled: false, engines: [] })
    await signInAs()
    renderWithProviders(<Pages />, { route: '/databases/db-0a1b2c3d4e5f60718' })

    expect(await screen.findByText('Databases are turned off on this node')).toBeInTheDocument()
  })
})

describe('local validation', () => {
  it.each([
    ['', 'Enter an identifier for the database.'],
    ['Orders', 'Must start with a letter'],
    ['1orders', 'Must start with a letter'],
    ['orders-', 'Must start with a letter'],
    ['or--ders', 'Must start with a letter'],
    ['or_ders', 'Must start with a letter'],
  ])('refuses the identifier %s', (identifier, reason) => {
    expect(identifierProblem(identifier)).toContain(reason)
  })

  it('accepts an ordinary identifier', () => {
    expect(identifierProblem('orders-eu-1')).toBeNull()
  })

  it.each([
    ['postgres', 'reserved'],
    ['pg_monitor', 'reserved'],
    ['template1', 'reserved'],
    ['My-App', 'Must start with a lowercase letter'],
    ['', 'Enter'],
  ])('refuses the Postgres name %s', (name, reason) => {
    expect(pgNameProblem(name, 'a name')).toContain(reason)
  })

  it('accepts an ordinary role name', () => {
    expect(pgNameProblem('app_rw', 'a name')).toBeNull()
  })

  // The ASCII rule is a correctness rule: the engine derives its credential
  // through SASLprep and Kumo does not, so a non-ASCII password authenticates
  // nowhere. Catching it here is what keeps it from looking accepted.
  it.each([
    ['short', 'at least 10'],
    ['passphrase—long', 'printable ASCII'],
    ['café au lait pass', 'printable ASCII'],
    ['tab\tseparated password', 'printable ASCII'],
  ])('refuses the password %s', (password, reason) => {
    expect(passwordProblem(password)).toContain(reason)
  })

  it('accepts a hostile but printable password', () => {
    expect(passwordProblem('Tr0ub4dor&3 \'quote"$dollar\\back')).toBeNull()
  })

  it.each([1, 0, 4, 2049, 20.5])('refuses the storage size %s', (size) => {
    expect(storageProblem(size)).toContain('between 5 and 2048')
  })

  it('accepts an ordinary storage size', () => {
    expect(storageProblem(20)).toBeNull()
  })
})
