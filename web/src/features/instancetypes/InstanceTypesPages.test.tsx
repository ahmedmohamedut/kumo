import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { InstanceTypesListPage } from '@/features/instancetypes/InstanceTypesListPage'
import {
  CreateInstanceTypePage,
  EditInstanceTypePage,
} from '@/features/instancetypes/InstanceTypeFormPage'
import { MOCK_VIEWER, mockInstanceTypes, setMockInstanceTypes } from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

function Pages() {
  return (
    <Routes>
      <Route path="/instance-types" element={<InstanceTypesListPage />} />
      <Route path="/instance-types/create" element={<CreateInstanceTypePage />} />
      <Route path="/instance-types/:instanceTypeId/edit" element={<EditInstanceTypePage />} />
      <Route path="/instances/:instanceId" element={<p>Instance detail</p>} />
    </Routes>
  )
}

/** rowOf finds the table row of an instance type. */
function rowOf(id: string): HTMLElement {
  const row = screen.getByText(id).closest('tr')
  if (row === null) {
    throw new Error(`no table row for ${id}`)
  }
  return row
}

/** select picks the row of an instance type. */
async function select(id: string) {
  await userEvent.click(within(rowOf(id)).getByRole('radio'))
}

/** replace clears an input and types a new value into it. */
async function replace(label: string, value: string) {
  const input = screen.getByLabelText(label)
  await userEvent.clear(input)
  if (value !== '') {
    await userEvent.type(input, value)
  }
}

describe('InstanceTypesListPage', () => {
  it('lists the types smallest first with their size', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types' })

    expect(await screen.findByText('kumo.small')).toBeInTheDocument()
    expect(within(rowOf('kumo.small')).getByText('4 GiB')).toBeInTheDocument()
    expect(within(rowOf('kumo.large')).getByText('16 GiB')).toBeInTheDocument()

    const ids = screen
      .getAllByRole('row')
      .slice(1)
      .map((row) => within(row).queryByText(/^kumo\./)?.textContent)
    expect(ids).toEqual(['kumo.small', 'kumo.medium', 'kumo.large'])
  })

  it('keeps the write actions away from a viewer', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/instance-types' })

    await screen.findByText('kumo.small')
    expect(screen.getByRole('button', { name: 'Create instance type' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Edit' })).toBeDisabled()
    expect(screen.getByTestId('delete-instance-type')).toBeDisabled()
    expect(screen.queryByRole('radio')).not.toBeInTheDocument()
  })

  it('enables Edit and Delete once a type is selected', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types' })

    await screen.findByText('kumo.large')
    expect(screen.getByRole('button', { name: 'Edit' })).toBeDisabled()

    await select('kumo.large')
    expect(screen.getByRole('button', { name: 'Edit' })).toBeEnabled()
    expect(screen.getByTestId('delete-instance-type')).toBeEnabled()

    await userEvent.click(screen.getByRole('button', { name: 'Edit' }))
    expect(await screen.findByRole('heading', { name: 'Edit kumo.large' })).toBeInTheDocument()
  })

  it('deletes an unused type once confirmed', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/instance-types' })

    await screen.findByText('kumo.large')
    await select('kumo.large')
    await userEvent.click(screen.getByTestId('delete-instance-type'))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('heading', { name: 'Delete kumo.large?' })).toBeInTheDocument()
    // The confirmation is not a side effect.
    expect(mockInstanceTypes().map((type) => type.id)).toContain('kumo.large')

    await waitFor(() => expect(within(dialog).getByTestId('confirm-delete')).toBeEnabled())
    await userEvent.click(within(dialog).getByTestId('confirm-delete'))

    await waitFor(() => expect(screen.queryByText('kumo.large')).not.toBeInTheDocument())
    expect(mockInstanceTypes().map((type) => type.id)).not.toContain('kumo.large')
    expect(store.getState().notifications.items[0]?.header).toBe('Instance type deleted')
  })

  it('names the instances that block a delete and offers no Delete', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types' })

    await screen.findByText('kumo.medium')
    await select('kumo.medium')
    await userEvent.click(screen.getByTestId('delete-instance-type'))

    const dialog = await screen.findByRole('dialog')
    expect(await within(dialog).findByText('This instance type is in use')).toBeInTheDocument()
    const blockers = within(dialog).getByRole('list', { name: 'Instances using this type' })
    expect(within(blockers).getAllByRole('link').length).toBeGreaterThan(0)
    expect(within(dialog).getByTestId('confirm-delete')).toBeDisabled()

    // A blocking instance is one click away.
    await userEvent.click(within(blockers).getAllByRole('link')[0] as HTMLElement)
    expect(await screen.findByText('Instance detail')).toBeInTheDocument()
    expect(mockInstanceTypes().map((type) => type.id)).toContain('kumo.medium')
  })

  it('leaves the type alone when the confirmation is cancelled', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types' })

    await screen.findByText('kumo.large')
    await select('kumo.large')
    await userEvent.click(screen.getByTestId('delete-instance-type'))
    const dialog = await screen.findByRole('dialog')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(mockInstanceTypes().map((type) => type.id)).toContain('kumo.large')
  })

  it('shows an empty state with the way to create one', async () => {
    setMockInstanceTypes([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types' })

    expect(await screen.findByRole('heading', { name: 'No instance types' })).toBeInTheDocument()
  })

  it('clears a filter that matches nothing', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types' })

    await screen.findByText('kumo.small')
    await userEvent.type(screen.getByLabelText('Filter instance types'), 'nothing-like-this')
    expect(await screen.findByRole('heading', { name: 'No matches' })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Clear filter' }))
    expect(await screen.findByText('kumo.small')).toBeInTheDocument()
  })
})

describe('CreateInstanceTypePage', () => {
  it('creates a type with a generated description and returns to the list', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/instance-types/create' })

    await userEvent.type(await screen.findByLabelText('Name'), 'db.large')
    await replace('vCPUs', '4')
    await replace('Memory (MiB)', '8192')
    await userEvent.click(screen.getByTestId('submit-instance-type'))

    expect(await screen.findByText('db.large')).toBeInTheDocument()
    expect(mockInstanceTypes().find((type) => type.id === 'db.large')).toEqual({
      id: 'db.large',
      vcpus: 4,
      memory_mib: 8192,
      description: '4 vCPU, 8 GiB',
    })
    expect(store.getState().notifications.items[0]?.header).toBe('Instance type created')
  })

  it('refuses invalid input before sending anything', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types/create' })

    await userEvent.type(await screen.findByLabelText('Name'), 'Big/One')
    await replace('vCPUs', '0')
    await replace('Memory (MiB)', '64')
    await userEvent.click(screen.getByTestId('submit-instance-type'))

    expect(
      await screen.findByText(
        "Use 1-64 lowercase letters, digits, '.', '_' or '-', starting with a letter or digit.",
      ),
    ).toBeInTheDocument()
    expect(screen.getByText('Enter a whole number between 1 and 256.')).toBeInTheDocument()
    expect(screen.getByText(/whole number of MiB between 256/)).toBeInTheDocument()
    expect(mockInstanceTypes()).toHaveLength(3)
  })

  it('reports a taken name from the API', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types/create' })

    await userEvent.type(await screen.findByLabelText('Name'), 'kumo.small')
    await userEvent.click(screen.getByTestId('submit-instance-type'))

    expect(await screen.findByText('The instance type was not saved')).toBeInTheDocument()
    expect(screen.getByText('an instance type "kumo.small" already exists')).toBeInTheDocument()
  })

  it('is read only for a viewer', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/instance-types/create' })

    expect(await screen.findByText('Read only')).toBeInTheDocument()
    expect(screen.getByTestId('submit-instance-type')).toBeDisabled()
  })
})

describe('EditInstanceTypePage', () => {
  it('starts from the stored values and keeps the name read only', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types/kumo.medium/edit' })

    expect(await screen.findByLabelText('Name')).toHaveValue('kumo.medium')
    expect(screen.getByLabelText('Name')).toHaveAttribute('readonly')
    expect(screen.getByLabelText('vCPUs')).toHaveValue(4)
    expect(screen.getByLabelText('Memory (MiB)')).toHaveValue(8192)
    expect(screen.getByLabelText('Description')).toHaveValue('Medium')
  })

  it('sends only what changed, so a generated description follows the size', async () => {
    setMockInstanceTypes([
      { id: 'kumo.small', vcpus: 1, memory_mib: 2048, description: '1 vCPU, 2 GiB' },
    ])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types/kumo.small/edit' })

    await screen.findByLabelText('vCPUs')
    await replace('vCPUs', '2')
    await userEvent.click(screen.getByTestId('submit-instance-type'))

    expect(await screen.findByRole('heading', { name: /Instance types/ })).toBeInTheDocument()
    expect(mockInstanceTypes()[0]).toEqual({
      id: 'kumo.small',
      vcpus: 2,
      memory_mib: 2048,
      description: '2 vCPU, 2 GiB',
    })
  })

  it('says so instead of sending an empty change', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types/kumo.large/edit' })

    await screen.findByLabelText('vCPUs')
    await userEvent.click(screen.getByTestId('submit-instance-type'))

    expect(await screen.findByText('Nothing to save')).toBeInTheDocument()
  })

  it('shows an API refusal on the field', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types/kumo.large/edit' })

    await screen.findByLabelText('Description')
    await replace('Description', 'x'.repeat(256))
    await userEvent.click(screen.getByTestId('submit-instance-type'))

    // The local check catches it first; nothing reaches the mock.
    expect(await screen.findByText('Use at most 255 characters.')).toBeInTheDocument()
    expect(mockInstanceTypes().find((type) => type.id === 'kumo.large')?.description).toBe('Large')
  })

  it('reports a type that no longer exists', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/instance-types/kumo.gone/edit' })

    expect(await screen.findByText('This instance type no longer exists')).toBeInTheDocument()
  })
})
