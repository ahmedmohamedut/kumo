import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { KeyPairsListPage } from '@/features/keypairs/KeyPairsListPage'
import { addMockUsedBy, mockKeyPairs, setMockKeyPairs } from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

function Pages() {
  return (
    <Routes>
      <Route path="/key-pairs" element={<KeyPairsListPage />} />
      <Route path="/key-pairs/create" element={<p>Create key pair</p>} />
      <Route path="/key-pairs/import" element={<p>Import key pair</p>} />
    </Routes>
  )
}

/** rowOf finds the table row holding a key pair name. */
function rowOf(name: string): HTMLElement {
  const cell = screen.getByRole('link', { name })
  const row = cell.closest('tr')
  if (row === null) {
    throw new Error(`no table row for ${name}`)
  }
  return row
}

describe('KeyPairsListPage', () => {
  it('lists the key pairs with their type and fingerprint', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs' })

    expect(await screen.findByRole('link', { name: 'deploy-bot' })).toBeInTheDocument()
    expect(within(rowOf('deploy-bot')).getByText('ED25519')).toBeInTheDocument()
    expect(
      within(rowOf('deploy-bot')).getByText('SHA256:nE1g6wUlVBQKTyKOSJF1xO4wZDGZSJaEgxVU2je12jg'),
    ).toBeInTheDocument()
    expect(within(rowOf('legacy-rsa')).getByText('RSA')).toBeInTheDocument()
    // The newest first, which is the API's default order.
    const names = screen.getAllByRole('link').map((link) => link.textContent)
    expect(names).toEqual(['deploy-bot', 'legacy-rsa'])
  })

  it('offers an empty state with both ways to add a key pair', async () => {
    setMockKeyPairs([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs' })

    expect(await screen.findByRole('heading', { name: 'No key pairs' })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Create key pair' }))
    expect(await screen.findByText('Create key pair')).toBeInTheDocument()
  })

  it('sends the operator to the import form', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs' })

    await screen.findByRole('link', { name: 'deploy-bot' })
    await userEvent.click(screen.getByRole('button', { name: 'Add key pair' }))
    await userEvent.click(await screen.findByRole('menuitem', { name: 'Import key pair' }))

    expect(await screen.findByText('Import key pair')).toBeInTheDocument()
  })

  it('filters server-side and keeps the filter in the URL', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs' })

    await screen.findByRole('link', { name: 'deploy-bot' })
    await userEvent.type(screen.getByLabelText('Filter key pairs'), 'legacy')

    await waitFor(() =>
      expect(screen.queryByRole('link', { name: 'deploy-bot' })).not.toBeInTheDocument(),
    )
    expect(screen.getByRole('link', { name: 'legacy-rsa' })).toBeInTheDocument()
  })

  it('asks before deleting, and only deletes once confirmed', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs' })

    await screen.findByRole('link', { name: 'deploy-bot' })
    // Nothing selected, nothing to delete.
    expect(screen.getByTestId('delete-key-pairs')).toBeDisabled()

    await userEvent.click(within(rowOf('deploy-bot')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-key-pairs'))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('heading', { name: 'Delete deploy-bot?' })).toBeInTheDocument()
    // The confirmation is not a side effect: nothing is gone yet.
    expect(mockKeyPairs().map((pair) => pair.name)).toContain('deploy-bot')

    await userEvent.click(screen.getByTestId('confirm-delete'))

    await waitFor(() =>
      expect(screen.queryByRole('link', { name: 'deploy-bot' })).not.toBeInTheDocument(),
    )
    expect(mockKeyPairs().map((pair) => pair.name)).not.toContain('deploy-bot')
  })

  it('leaves the key pair alone when the confirmation is cancelled', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs' })

    await screen.findByRole('link', { name: 'deploy-bot' })
    await userEvent.click(within(rowOf('deploy-bot')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-key-pairs'))

    const dialog = await screen.findByRole('dialog')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(mockKeyPairs().map((pair) => pair.name)).toContain('deploy-bot')
  })

  it('reports the 409 for a key pair a running instance is using', async () => {
    // legacy-rsa is already attached to i-0a1b2c3d4e5f60718 in the fixtures.
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/key-pairs' })

    await screen.findByRole('link', { name: 'legacy-rsa' })
    await userEvent.click(within(rowOf('legacy-rsa')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-key-pairs'))
    await userEvent.click(screen.getByTestId('confirm-delete'))

    // The row is still there, and the Flashbar says which key and why.
    expect(await screen.findByRole('link', { name: 'legacy-rsa' })).toBeInTheDocument()
    const notifications = store.getState().notifications.items
    expect(notifications).toHaveLength(1)
    expect(notifications[0]?.type).toBe('error')
    expect(notifications[0]?.content).toContain('legacy-rsa')
    expect(notifications[0]?.content).toContain('i-0a1b2c3d4e5f60718')
  })

  it('deletes an in-use key pair when the operator forces it', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs' })

    await screen.findByRole('link', { name: 'legacy-rsa' })
    await userEvent.click(within(rowOf('legacy-rsa')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-key-pairs'))

    const dialog = await screen.findByRole('dialog')
    await userEvent.click(within(dialog).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('confirm-delete'))

    await waitFor(() =>
      expect(screen.queryByRole('link', { name: 'legacy-rsa' })).not.toBeInTheDocument(),
    )
    expect(mockKeyPairs().map((pair) => pair.name)).not.toContain('legacy-rsa')
  })

  it('re-reads a key pair an instance started using since the last poll', async () => {
    // The list cannot know on its own, so the 409 is what surfaces it.
    addMockUsedBy('key-0a1b2c3d4e5f60718', 'i-0fffffffffffffff')
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/key-pairs' })

    await screen.findByRole('link', { name: 'deploy-bot' })
    await userEvent.click(within(rowOf('deploy-bot')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-key-pairs'))
    await userEvent.click(screen.getByTestId('confirm-delete'))

    expect(await screen.findByRole('link', { name: 'deploy-bot' })).toBeInTheDocument()
    const notifications = store.getState().notifications.items
    expect(notifications[0]?.type).toBe('error')
    expect(notifications[0]?.content).toContain('i-0fffffffffffffff')
  })
})
