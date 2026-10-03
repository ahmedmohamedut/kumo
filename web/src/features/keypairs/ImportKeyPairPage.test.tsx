import { describe, expect, it } from 'vitest'
import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { ImportKeyPairPage } from '@/features/keypairs/ImportKeyPairPage'
import { mockKeyPairs } from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

/** A real ED25519 authorized_keys line: the output of `cat ~/.ssh/id_ed25519.pub`. */
const PUBLIC_KEY =
  'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIMSkpB4KwlcQbdHTekHMstz+XeDU0Eugj1nOCRMlwz0e ada@laptop'

/** The fragment an operator grabs when they pick the wrong file. */
const PRIVATE_KEY =
  '-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQ\n-----END OPENSSH PRIVATE KEY-----'

function Pages() {
  return (
    <Routes>
      <Route path="/key-pairs/import" element={<ImportKeyPairPage />} />
      <Route path="/key-pairs" element={<p>Key pairs list</p>} />
    </Routes>
  )
}

async function importWith(name: string, publicKey: string) {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Key pair name'), name)
  await user.type(screen.getByLabelText('Public key'), publicKey)
  await user.click(screen.getByTestId('import-key-pair'))
}

describe('ImportKeyPairPage', () => {
  it('imports a public key and returns to the list', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/key-pairs/import' })

    await importWith('laptop', PUBLIC_KEY)

    expect(await screen.findByText('Key pairs list')).toBeInTheDocument()
    const imported = mockKeyPairs().find((pair) => pair.name === 'laptop')
    expect(imported?.public_key).toBe(PUBLIC_KEY)
    expect(imported?.type).toBe('ed25519')
    expect(store.getState().notifications.items[0]?.type).toBe('success')
  })

  it('refuses private key material and says why, on the field', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs/import' })

    await importWith('oops', PRIVATE_KEY)

    expect(await screen.findByText('private key material must not be imported')).toBeInTheDocument()
    expect(mockKeyPairs().some((pair) => pair.name === 'oops')).toBe(false)
    // Still on the form, so the operator can fix the paste.
    expect(screen.getByTestId('import-key-pair')).toBeInTheDocument()
  })

  it('rejects a value that is not an OpenSSH public key', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs/import' })

    await importWith('junk', 'this is not a key')

    expect(await screen.findByText('is not an OpenSSH public key')).toBeInTheDocument()
    expect(mockKeyPairs().some((pair) => pair.name === 'junk')).toBe(false)
  })

  it('rejects a name that is already taken', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs/import' })

    await importWith('deploy-bot', PUBLIC_KEY)

    expect(await screen.findByText(/already exists/)).toBeInTheDocument()
    expect(mockKeyPairs().filter((pair) => pair.name === 'deploy-bot')).toHaveLength(1)
  })

  it('rejects a name the API will not accept', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs/import' })

    await importWith('not a valid name', PUBLIC_KEY)

    expect(await screen.findByText(/must be 1-64 characters/)).toBeInTheDocument()
    expect(mockKeyPairs().some((pair) => pair.name === 'not a valid name')).toBe(false)
  })

  it('cancels back to the list without importing anything', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/key-pairs/import' })

    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))

    expect(await screen.findByText('Key pairs list')).toBeInTheDocument()
    expect(mockKeyPairs()).toHaveLength(2)
  })
})
