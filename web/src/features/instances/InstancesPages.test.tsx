import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { InstancesListPage } from '@/features/instances/InstancesListPage'
import { LaunchInstancePage } from '@/features/instances/LaunchInstancePage'
import { InstanceDetailPage } from '@/features/instances/InstanceDetailPage'
import {
  findMockInstance,
  mockInstances,
  setMockInstances,
  setMockKeyPairs,
} from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

function Pages() {
  return (
    <Routes>
      <Route path="/instances" element={<InstancesListPage />} />
      <Route path="/instances/launch" element={<LaunchInstancePage />} />
      <Route path="/instances/:instanceId" element={<InstanceDetailPage />} />
      <Route path="/key-pairs/create" element={<p>Create key pair</p>} />
    </Routes>
  )
}

/** DETAIL renders the detail page for one fixture instance. */
function detailOf(instanceId: string) {
  return renderWithProviders(<Pages />, { route: `/instances/${instanceId}` })
}

/** rowOf finds the table row holding an instance name. */
function rowOf(name: string): HTMLElement {
  const cell = screen.getByRole('link', { name })
  const row = cell.closest('tr')
  if (row === null) {
    throw new Error(`no table row for ${name}`)
  }
  return row
}

/** selectRow picks an instance's row. It waits for the row rather than asserting
 *  on a `getByRole`, because the list is still loading on the first paint and the
 *  row is what the test is actually about.
 *
 *  `selectionType="single"` renders a radio, not a checkbox, so the control is
 *  queried as a radio.
 */
async function selectRow(name: string) {
  const cell = await screen.findByRole('link', { name })
  const row = cell.closest('tr')
  if (row === null) {
    throw new Error(`no table row for ${name}`)
  }
  await userEvent.click(within(row).getByRole('radio'))
}

/** actionsButton is the lifecycle dropdown trigger. ButtonDropdown puts a
 *  `data-testid` on its wrapper rather than the inner button, so this queries the
 *  trigger by its accessible name instead. */
function actionsButton(): HTMLElement {
  return screen.getByRole('button', { name: 'Instance state' })
}

/** openActions opens the lifecycle dropdown for the current selection. */
async function openActions() {
  await userEvent.click(actionsButton())
}

/**
 * menuItem is one entry of the opened lifecycle dropdown.
 *
 * A disabled menu entry is a `<span role="menuitem" aria-disabled="true">`, not a
 * `<button>`, so `toBeDisabled` (which reads the `disabled` attribute) reports it
 * as enabled. The assertion is on `aria-disabled`, which is what actually stops
 * the click and what a screen reader announces.
 */
async function menuItem(label: string) {
  return screen.findByRole('menuitem', { name: label })
}

/** itemEnabled is the honest check for a dropdown entry's availability. */
function itemEnabled(item: HTMLElement): boolean {
  return item.getAttribute('aria-disabled') !== 'true'
}

describe('InstancesListPage', () => {
  it('lists the instances with their state and the denormalised names', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances' })

    expect(await screen.findByRole('link', { name: 'web-1' })).toBeInTheDocument()

    // The state cell carries the indicator, not just the name.
    const running = within(rowOf('web-1'))
    expect(running.getByText('Running')).toBeInTheDocument()
    expect(running.getByText('kumo.small')).toBeInTheDocument()
    expect(running.getByText('10.0.0.11')).toBeInTheDocument()
    expect(running.getByText('alpine-3.20')).toBeInTheDocument()
    expect(running.getByText('legacy-rsa')).toBeInTheDocument()

    // Newest first by launched_at: web-2 (Jan 5), db-1 (Jan 4), web-1 (Jan 3),
    // old-1 (Jan 2). The terminated row is last because it is the oldest.
    const names = screen.getAllByRole('link').map((link) => link.textContent)
    expect(names).toEqual(['web-2', 'db-1', 'web-1', 'old-1'])
  })

  it('shows the reason an instance went to error, not just "Error"', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances' })

    expect(await screen.findByRole('link', { name: 'db-1' })).toBeInTheDocument()
    const failed = within(rowOf('db-1'))
    expect(failed.getByText('Error')).toBeInTheDocument()
    // This is the whole reason the state cell is its own component: a table that
    // says only "Error" sends the operator to the node to guess.
    expect(failed.getByText(/root disk 2 GiB is below the 8 GiB/)).toBeInTheDocument()
  })

  it('keeps a terminated instance in the list as history', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances' })

    expect(await screen.findByRole('link', { name: 'old-1' })).toBeInTheDocument()
    expect(within(rowOf('old-1')).getByText('Terminated')).toBeInTheDocument()
  })

  it('offers an empty state that leads to the launch form', async () => {
    setMockInstances([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances' })

    expect(await screen.findByRole('heading', { name: 'No instances' })).toBeInTheDocument()
    // Both the header action and the empty state's call to action read "Launch
    // instance". The first one is what this test is about, since it is the path an
    // operator takes once the table is populated.
    await userEvent.click(
      screen.getAllByRole('button', { name: 'Launch instance' })[0] as HTMLElement,
    )

    expect(
      await screen.findByRole('heading', { level: 1, name: 'Launch instance' }),
    ).toBeInTheDocument()
  })

  it('filters server-side and keeps the filter in the URL', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances' })

    await screen.findByRole('link', { name: 'web-1' })
    await userEvent.type(screen.getByLabelText('Filter instances'), 'db')

    await waitFor(() =>
      expect(screen.queryByRole('link', { name: 'web-1' })).not.toBeInTheDocument(),
    )
    expect(screen.getByRole('link', { name: 'db-1' })).toBeInTheDocument()
  })

  it('disables every action until an instance is selected', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances' })

    await screen.findByRole('link', { name: 'web-1' })
    // Nothing selected, so there is nothing to act on.
    expect(actionsButton()).toBeDisabled()
  })

  it('only offers the actions the current state allows', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances' })

    // web-1 is running: Start is the one that is refused, and it is shown
    // disabled rather than hidden so the operator can see it exists.
    await selectRow('web-1')
    await openActions()

    expect(itemEnabled(await menuItem('Stop'))).toBe(true)
    expect(itemEnabled(await menuItem('Force stop'))).toBe(true)
    expect(itemEnabled(await menuItem('Reboot'))).toBe(true)
    expect(itemEnabled(await menuItem('Terminate'))).toBe(true)
    // Shown disabled rather than hidden, so the operator can see Start exists.
    expect(itemEnabled(await menuItem('Start'))).toBe(false)
  })

  it('stops a running instance and reports it as a background task', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/instances' })

    await screen.findByRole('link', { name: 'web-1' })
    await selectRow('web-1')
    await openActions()
    await userEvent.click(await menuItem('Stop'))

    // A graceful stop needs no confirmation; the row lands in `stopping`.
    await waitFor(() => expect(findMockInstance('i-0a1b2c3d4e5f60718')?.state).toBe('stopping'))
    const notifications = store.getState().notifications.items
    expect(notifications[0]?.type).toBe('success')
    // 202 means accepted, not finished: the wording must not claim the guest is off.
    expect(notifications[0]?.content).toContain('in the background')
  })

  it('reboots a running instance', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances' })

    await screen.findByRole('link', { name: 'web-1' })
    await selectRow('web-1')
    await openActions()
    await userEvent.click(await menuItem('Reboot'))

    await waitFor(() => expect(mockInstances().length).toBeGreaterThan(0))
  })

  it('asks for the name before terminating, and cancels cleanly', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances' })

    await screen.findByRole('link', { name: 'web-1' })
    await selectRow('web-1')
    await openActions()
    await userEvent.click(await menuItem('Terminate'))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('heading', { name: 'Terminate web-1?' })).toBeInTheDocument()
    // Nothing is gone yet, and the button stays shut until the name matches.
    expect(screen.getByTestId('confirm-destructive')).toBeDisabled()
    expect(findMockInstance('i-0a1b2c3d4e5f60718')?.state).toBe('running')

    // A wrong name does not unlock it.
    await userEvent.type(screen.getByRole('textbox', { name: 'Type the name to confirm' }), 'web-2')
    expect(screen.getByTestId('confirm-destructive')).toBeDisabled()

    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(findMockInstance('i-0a1b2c3d4e5f60718')?.state).toBe('running')
  })

  it('terminates once the name is typed exactly', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances' })

    await screen.findByRole('link', { name: 'web-1' })
    await selectRow('web-1')
    await openActions()
    await userEvent.click(await menuItem('Terminate'))

    await userEvent.type(
      await screen.findByRole('textbox', { name: 'Type the name to confirm' }),
      'web-1',
    )
    await userEvent.click(screen.getByTestId('confirm-destructive'))

    await waitFor(() => expect(findMockInstance('i-0a1b2c3d4e5f60718')?.state).toBe('terminating'))
  })

  it('confirms a force stop without asking for the name', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances' })

    await screen.findByRole('link', { name: 'web-1' })
    await selectRow('web-1')
    await openActions()
    await userEvent.click(await menuItem('Force stop'))

    const dialog = await screen.findByRole('dialog')
    // Force stop is destructive but nothing is deleted, so no typing is required.
    expect(within(dialog).getByRole('heading', { name: 'Force stop web-1?' })).toBeInTheDocument()
    expect(
      within(dialog).queryByRole('textbox', { name: 'Type the name to confirm' }),
    ).not.toBeInTheDocument()
    expect(screen.getByTestId('confirm-destructive')).toBeEnabled()

    await userEvent.click(screen.getByTestId('confirm-destructive'))
    await waitFor(() => expect(findMockInstance('i-0a1b2c3d4e5f60718')?.state).toBe('stopping'))
  })

  it('offers nothing at all on a terminated instance', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances' })

    // A terminated instance has no domain left to act on. Every entry is disabled
    // rather than hidden, so the operator can see the whole action set and that
    // none of it applies.
    await screen.findByRole('link', { name: 'old-1' })
    await selectRow('old-1')
    await openActions()
    // old-1 is terminated: there is nothing left to start, stop, reboot or remove.
    expect(itemEnabled(await menuItem('Start'))).toBe(false)
    expect(itemEnabled(await menuItem('Stop'))).toBe(false)
    expect(itemEnabled(await menuItem('Reboot'))).toBe(false)
    expect(itemEnabled(await menuItem('Terminate'))).toBe(false)
  })
})

describe('LaunchInstancePage', () => {
  /**
   * pickImage chooses an image from the picker.
   *
   * Cloudscape's Select is a closed combobox, so the trigger button showing the
   * `placeholder` text is clicked first and the option is then picked from the
   * listbox that opens.
   */
  async function pickImage(label: RegExp) {
    await userEvent.click(screen.getByRole('button', { name: /Choose an image/ }))
    await userEvent.click(await screen.findByRole('option', { name: label }))
  }

  /** pickPreset chooses an instance type. */
  async function pickPreset(label: RegExp) {
    await userEvent.click(screen.getByRole('button', { name: /Choose an instance type/ }))
    await userEvent.click(await screen.findByRole('option', { name: label }))
  }

  it('seeds the root disk from the image minimum', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances/launch' })

    await waitFor(() =>
      expect(screen.getByRole('button', { name: /Choose an image/ })).toBeInTheDocument(),
    )
    await pickImage(/alpine-3\.20/)

    // alpine's fixture min_disk_gb is 1, so the field is seeded rather than blank.
    await waitFor(() =>
      expect(screen.getByRole('spinbutton', { name: 'Root disk' })).toHaveValue(1),
    )
  })

  it('reports the bad fields without a request', async () => {
    setMockKeyPairs([])
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/instances/launch' })

    await waitFor(() =>
      expect(screen.getByRole('button', { name: /Choose an image/ })).toBeInTheDocument(),
    )
    // Nothing chosen, so nothing is enqueued — the form says so instead.
    await userEvent.click(screen.getByTestId('submit-launch'))

    expect(await screen.findByText('Enter a name for the instance.')).toBeInTheDocument()
    expect(screen.getByText('Choose an image to launch from.')).toBeInTheDocument()
    expect(screen.getByText('Choose an instance type.')).toBeInTheDocument()
    expect(store.getState().notifications.items).toHaveLength(0)
    expect(mockInstances()).toHaveLength(4)
  })

  it('refuses a name that is not an RFC 1123 label', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances/launch' })

    await waitFor(() =>
      expect(screen.getByRole('button', { name: /Choose an image/ })).toBeInTheDocument(),
    )
    await userEvent.type(screen.getByRole('textbox', { name: 'Name' }), 'Web_1!')
    await userEvent.click(screen.getByTestId('submit-launch'))

    expect(
      await screen.findByText(/must be 1-63 characters of lowercase letters, digits and dashes/i),
    ).toBeInTheDocument()
  })

  it('refuses a root disk below what the image needs', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances/launch' })

    await waitFor(() =>
      expect(screen.getByRole('button', { name: /Choose an image/ })).toBeInTheDocument(),
    )
    await pickImage(/ubuntu-22\.04/)
    // ubuntu-22.04's fixture min_disk_gb is 8, so the field is seeded with the
    // floor rather than a default of 10.
    await waitFor(() =>
      expect(screen.getByRole('spinbutton', { name: 'Root disk' })).toHaveValue(8),
    )

    await userEvent.type(screen.getByRole('textbox', { name: 'Name' }), 'web-9')
    await pickPreset(/kumo\.small/)
    await userEvent.clear(screen.getByRole('spinbutton', { name: 'Root disk' }))
    await userEvent.type(screen.getByRole('spinbutton', { name: 'Root disk' }), '4')
    await userEvent.click(screen.getByTestId('submit-launch'))

    expect(await screen.findByText(/ubuntu-22.04 needs at least 8 GiB/)).toBeInTheDocument()
  })

  it('launches and goes to the instance detail page', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/instances/launch' })

    await waitFor(() =>
      expect(screen.getByRole('button', { name: /Choose an image/ })).toBeInTheDocument(),
    )
    await userEvent.type(screen.getByRole('textbox', { name: 'Name' }), 'web-9')
    await pickImage(/alpine-3\.20/)
    await pickPreset(/kumo\.small/)
    await userEvent.click(screen.getByTestId('submit-launch'))

    // The detail page, not a spinner: the launch is a task, so the page follows
    // the new instance rather than waiting on a boot.
    expect(await screen.findByRole('heading', { level: 1, name: 'web-9' })).toBeInTheDocument()
    const launched = mockInstances().find((instance) => instance.name === 'web-9')
    expect(launched?.state).toBe('pending')
    expect(store.getState().notifications.items[0]?.type).toBe('success')
    expect(store.getState().notifications.items[0]?.content).toContain(launched?.id)
  })

  it('shows the API 409 on the field that caused it', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances/launch' })

    await waitFor(() =>
      expect(screen.getByRole('button', { name: /Choose an image/ })).toBeInTheDocument(),
    )
    // web-1 already exists among the live fixtures.
    await userEvent.type(screen.getByRole('textbox', { name: 'Name' }), 'web-1')
    await pickImage(/alpine-3\.20/)
    await pickPreset(/kumo\.small/)
    await userEvent.click(screen.getByTestId('submit-launch'))

    expect(await screen.findByText(/an instance named "web-1" already exists/)).toBeInTheDocument()
  })

  it('reports a duplicate name that is only freed by terminating the old instance', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances/launch' })

    await waitFor(() =>
      expect(screen.getByRole('button', { name: /Choose an image/ })).toBeInTheDocument(),
    )
    // old-1 is terminated, so its name is free again (02-data-model.md).
    await userEvent.type(screen.getByRole('textbox', { name: 'Name' }), 'old-1')
    await pickImage(/alpine-3\.20/)
    await pickPreset(/kumo\.small/)
    await userEvent.click(screen.getByTestId('submit-launch'))

    expect(await screen.findByRole('heading', { level: 1, name: 'old-1' })).toBeInTheDocument()
    // Two rows now carry the name: the terminated history and the new instance.
    expect(mockInstances().filter((instance) => instance.name === 'old-1')).toHaveLength(2)
  })

  it('tells the operator to import an image first when there is none', async () => {
    setMockInstances([])
    await signInAs()
    // An empty instance list does not empty the image list, so this asserts the
    // picker exists and offers only available images.
    renderWithProviders(<Pages />, { route: '/instances/launch' })

    await waitFor(() =>
      expect(screen.getByRole('button', { name: /Choose an image/ })).toBeInTheDocument(),
    )
    await userEvent.click(screen.getByRole('button', { name: /Choose an image/ }))
    const options = await screen.findAllByRole('option')
    // Only images that finished importing can launch, so the picker never offers
    // one that is still downloading.
    expect(options.map((option) => option.textContent).join(' ')).toContain('alpine-3.20')
  })

  it('offers to proceed without a key pair, which is the default', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances/launch' })

    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: /Proceed without a key pair/ }),
      ).toBeInTheDocument(),
    )
    expect(screen.getByText('Proceed without a key pair')).toBeInTheDocument()
  })

  it('links to the key pair form without leaving the launch form behind', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instances/launch' })

    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: /Proceed without a key pair/ }),
      ).toBeInTheDocument(),
    )
    // `external` opens a new tab, so this must not navigate the console away.
    const link = screen.getByRole('link', { name: 'Create a new key pair' })
    expect(link).toHaveAttribute('href', '/key-pairs/create')
    expect(link).toHaveAttribute('target', '_blank')
  })
})
describe('InstanceDetailPage', () => {
  const WEB_1 = 'i-0a1b2c3d4e5f60718'
  const DB_1 = 'i-0c2d3e4f5a6071819'
  const OLD_1 = 'i-0d2e3f4a5b6071829'

  /** openTab clicks a tab in the detail page's tab bar. */
  async function openTab(name: string) {
    await userEvent.click(screen.getByRole('tab', { name }))
  }

  it('shows the name as the heading and the id as selectable text', async () => {
    await signInAs()
    detailOf(WEB_1)

    expect(await screen.findByRole('heading', { level: 1, name: 'web-1' })).toBeInTheDocument()
    // The id is what an operator pastes into `virsh`, so it must be copyable text
    // in the header, not something only visible on hover. It appears there and
    // again on the Details tab, hence the plural.
    expect(screen.getAllByText(WEB_1).length).toBeGreaterThan(0)
  })

  it('renders the details the operator needs to size the guest', async () => {
    await signInAs()
    detailOf(WEB_1)

    await screen.findByRole('heading', { level: 1, name: 'web-1' })
    expect(screen.getByText('kumo.small')).toBeInTheDocument()
    expect(screen.getByText('10.0.0.11')).toBeInTheDocument()
    // The image and key pair are links, because both are navigable resources.
    expect(screen.getByRole('link', { name: 'alpine-3.20' })).toHaveAttribute(
      'href',
      '/images/img-0d3e4f5a6071829',
    )
  })

  it('puts the failure reason in an alert, not just in the state', async () => {
    await signInAs()
    detailOf(DB_1)

    // A state cell saying "Error" would send the operator to the node to guess, so
    // the reason is spelled out where they are already looking. Cloudscape renders
    // an Alert's header as a div rather than a heading, hence the text query.
    expect(await screen.findByText('The instance is in an error state')).toBeInTheDocument()
    // Twice: once in the alert, once under the state indicator.
    expect(screen.getAllByText(/root disk 2 GiB is below the 8 GiB/)).toHaveLength(2)
  })

  it('reports a missing instance instead of rendering an empty page', async () => {
    await signInAs()
    detailOf('i-00000000000000000')

    expect(await screen.findByText('The instance could not be loaded')).toBeInTheDocument()
    expect(screen.getByText(/no instance/)).toBeInTheDocument()
  })

  it('shows the root disk on the storage tab and admits it cannot be resized', async () => {
    await signInAs()
    detailOf(WEB_1)

    await screen.findByRole('heading', { level: 1, name: 'web-1' })
    await openTab('Storage')

    expect(await screen.findByText('8 GiB')).toBeInTheDocument()
    expect(screen.getByText('Not available yet')).toBeInTheDocument()
  })

  it('lists the tags and edits them', async () => {
    await signInAs()
    detailOf(WEB_1)

    await screen.findByRole('heading', { level: 1, name: 'web-1' })
    await openTab('Tags')

    expect(await screen.findByText('prod')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Edit tags' }))
    // The editor opens seeded with what the instance already carries.
    expect(screen.getByDisplayValue('env')).toBeInTheDocument()
    expect(screen.getByDisplayValue('prod')).toBeInTheDocument()
  })

  it('says so plainly when an instance has no tags', async () => {
    await signInAs()
    detailOf(OLD_1)

    await screen.findByRole('heading', { level: 1, name: 'old-1' })
    await openTab('Tags')

    expect(await screen.findByText('This instance has no tags.')).toBeInTheDocument()
  })

  it('renders the serial console tail and refreshes it on request', async () => {
    await signInAs()
    detailOf(WEB_1)

    await screen.findByRole('heading', { level: 1, name: 'web-1' })
    await openTab('Console output')

    const output = await screen.findByTestId('console-output')
    expect(output.textContent).toContain('cloud-init')

    await userEvent.click(screen.getByRole('button', { name: 'Refresh console output' }))
    // The refresh must not blank the pane: an operator reading a boot log loses
    // their place if the text disappears while the request is in flight.
    expect(screen.getByTestId('console-output')).toBeInTheDocument()
  })

  it('offers a terminate that asks for the name', async () => {
    await signInAs()
    detailOf(WEB_1)

    await screen.findByRole('heading', { level: 1, name: 'web-1' })
    await openActions()
    await userEvent.click(await menuItem('Terminate'))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('heading', { name: 'Terminate web-1?' })).toBeInTheDocument()
    expect(screen.getByTestId('confirm-destructive')).toBeDisabled()

    await userEvent.type(screen.getByRole('textbox', { name: 'Type the name to confirm' }), 'web-1')
    await userEvent.click(screen.getByTestId('confirm-destructive'))

    await waitFor(() => expect(findMockInstance(WEB_1)?.state).toBe('terminating'))
  })

  it('offers a force stop that does not ask for the name', async () => {
    await signInAs()
    detailOf(WEB_1)

    await screen.findByRole('heading', { level: 1, name: 'web-1' })
    await openActions()
    await userEvent.click(await menuItem('Force stop'))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('heading', { name: 'Force stop web-1?' })).toBeInTheDocument()
    expect(
      within(dialog).queryByRole('textbox', { name: 'Type the name to confirm' }),
    ).not.toBeInTheDocument()
    // Nothing is deleted, so the button is live immediately.
    expect(screen.getByTestId('confirm-destructive')).toBeEnabled()

    await userEvent.click(screen.getByTestId('confirm-destructive'))
    await waitFor(() => expect(findMockInstance(WEB_1)?.state).toBe('stopping'))
  })

  it('disables every action on a terminated instance', async () => {
    await signInAs()
    detailOf(OLD_1)

    await screen.findByRole('heading', { level: 1, name: 'old-1' })
    await openActions()

    // The row survives as history; there is nothing left to act on.
    expect(itemEnabled(await menuItem('Start'))).toBe(false)
    expect(itemEnabled(await menuItem('Stop'))).toBe(false)
    expect(itemEnabled(await menuItem('Reboot'))).toBe(false)
    expect(itemEnabled(await menuItem('Terminate'))).toBe(false)
  })
})
