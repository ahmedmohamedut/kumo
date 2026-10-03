import { describe, expect, it, vi } from 'vitest'
import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { CreateKeyPairPage } from '@/features/keypairs/CreateKeyPairPage'
import { MOCK_PRIVATE_KEY, mockKeyPairs } from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

/** The create form plus the list it returns to once the key is saved. */
function Pages() {
  return (
    <Routes>
      <Route path="/key-pairs/create" element={<CreateKeyPairPage />} />
      <Route path="/key-pairs" element={<p>Key pairs list</p>} />
    </Routes>
  )
}

async function createWithName(name: string) {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Key pair name'), name)
  await user.click(screen.getByTestId('create-key-pair'))
}

/**
 * storeText is everything the console keeps about a request, flattened to text.
 * It exists so the "no private key in the store" assertions do not depend on
 * which RTK Query slice happened to hold the response.
 */
function storeText(store: { getState: () => unknown }): string {
  return JSON.stringify(store.getState())
}

describe('CreateKeyPairPage', () => {
  it('shows the generated private key once, and only lets it close once it is saved', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs/create' })

    await createWithName('deploy-bot-2')

    const dialog = await screen.findByRole('dialog')
    expect(dialog).toBeInTheDocument()
    expect(screen.getByLabelText('Private key')).toHaveValue(MOCK_PRIVATE_KEY)
    expect(mockKeyPairs().map((pair) => pair.name)).toContain('deploy-bot-2')

    // The key is unrecoverable, so the dialog must not close until the operator
    // says they saved it.
    expect(screen.getByTestId('private-key-done')).toBeDisabled()

    // Escape and the close icon both route through the same guard.
    await userEvent.keyboard('{Escape}')
    expect(screen.getByRole('dialog')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('checkbox'))
    expect(screen.getByTestId('private-key-done')).toBeEnabled()
  })

  it('keeps the private key out of the store, before and after the dialog closes', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/key-pairs/create' })

    await createWithName('deploy-bot-3')
    await screen.findByLabelText('Private key')

    // The dialog is open and the key is on screen, yet the store must not have it.
    expect(storeText(store)).not.toContain('PRIVATE KEY')

    await userEvent.click(screen.getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('private-key-done'))

    await waitFor(() => expect(screen.queryByLabelText('Private key')).not.toBeInTheDocument())
    expect(storeText(store)).not.toContain('PRIVATE KEY')
  })

  it('cannot show the private key again after the dialog is gone', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs/create' })

    await createWithName('deploy-bot-4')
    await screen.findByLabelText('Private key')

    await userEvent.click(screen.getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('private-key-done'))

    // Back on the list, and there is no route, button or re-render that brings
    // the key back: the server never stored it, so there is nothing to fetch.
    expect(await screen.findByText('Key pairs list')).toBeInTheDocument()
    expect(screen.queryByLabelText('Private key')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain('PRIVATE KEY')
  })

  it('downloads the key as <name>.pem', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs/create' })

    const createObjectURL = vi
      .spyOn(URL, 'createObjectURL')
      .mockReturnValue('blob:kumo-private-key')
    const revokeObjectURL = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    const clicked: string[] = []
    const clickSpy = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      clicked.push(this.download)
    })

    try {
      await createWithName('download-me')
      await screen.findByLabelText('Private key')
      await userEvent.click(screen.getByTestId('private-key-download'))

      expect(clicked).toEqual(['download-me.pem'])
      // The blob URL must not outlive the click, or the key stays in memory for
      // the life of the document.
      expect(revokeObjectURL).toHaveBeenCalledWith('blob:kumo-private-key')
    } finally {
      clickSpy.mockRestore()
      createObjectURL.mockRestore()
      revokeObjectURL.mockRestore()
    }
  })

  it('puts a rejected name on the field that caused it', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs/create' })

    await createWithName('not a valid name')

    expect(await screen.findByText(/must be 1-64 characters/)).toBeInTheDocument()
    expect(screen.getByTestId('create-key-pair')).toBeInTheDocument()
  })

  it('reports a duplicate name instead of creating a second key pair', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs/create' })

    // deploy-bot is in the fixture table.
    await createWithName('deploy-bot')

    expect(await screen.findByText(/already exists/)).toBeInTheDocument()
    expect(mockKeyPairs().filter((pair) => pair.name === 'deploy-bot')).toHaveLength(1)
  })
})
