import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { AccessKeysListPage } from '@/features/accesskeys/AccessKeysListPage'
import { CreateAccessKeyPage, EditAccessKeyPage } from '@/features/accesskeys/AccessKeyFormPage'
import { MOCK_VIEWER, mockAccessKeys, setMockObjectStorage } from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

function Pages() {
  return (
    <Routes>
      <Route path="/access-keys" element={<AccessKeysListPage />} />
      <Route path="/access-keys/create" element={<CreateAccessKeyPage />} />
      <Route path="/access-keys/:accessKeyId/edit" element={<EditAccessKeyPage />} />
    </Routes>
  )
}

function rowOf(name: string): HTMLElement {
  const row = screen.getByText(name).closest('tr')
  if (row === null) {
    throw new Error(`no row for ${name}`)
  }
  return row
}

async function pickBucket(name: string) {
  await userEvent.click(screen.getByRole('button', { name: /Buckets/ }))
  await userEvent.click(await screen.findByText(name, { selector: '[role="option"] *' }))
}

describe('AccessKeysListPage', () => {
  it('lists the keys without any secret', async () => {
    await signInAs()
    const { container } = renderWithProviders(<Pages />, { route: '/access-keys' })

    expect(await screen.findByText('log-shipper')).toBeInTheDocument()
    const row = rowOf('log-shipper')
    expect(within(row).getByText('KUMOAAAABBBBCCCCDDDD')).toBeInTheDocument()
    expect(within(row).getByText('Read and write')).toBeInTheDocument()
    expect(within(row).getByText('app-logs')).toBeInTheDocument()
    expect(within(row).getByText('Active')).toBeInTheDocument()
    expect(container.textContent).not.toMatch(/secret/i)
  })

  it('explains object storage is off', async () => {
    setMockObjectStorage({ backend: 'none', ok: false })
    await signInAs()
    renderWithProviders(<Pages />, { route: '/access-keys' })
    expect(
      await screen.findByText('Object storage is not configured on this node'),
    ).toBeInTheDocument()
  })

  it('keeps a viewer read-only', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/access-keys' })
    await screen.findByText('log-shipper')
    expect(screen.getByRole('button', { name: 'Create access key' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Edit' })).toBeDisabled()
    expect(screen.getByTestId('delete-access-key')).toBeDisabled()
  })

  it('deletes a key after confirmation', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/access-keys' })
    await screen.findByText('log-shipper')

    await userEvent.click(within(rowOf('log-shipper')).getByRole('radio'))
    await userEvent.click(screen.getByTestId('delete-access-key'))
    await userEvent.click(await screen.findByTestId('confirm-delete-access-key'))

    await waitFor(() => expect(mockAccessKeys()).toHaveLength(0))
    expect(await screen.findByText('No access keys')).toBeInTheDocument()
  })
})

describe('CreateAccessKeyPage', () => {
  it('issues a key and shows its secret once, until saved', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/access-keys/create' })

    await userEvent.type(await screen.findByLabelText('Name'), 'web-app')
    await userEvent.click(screen.getByLabelText(/Read and write/))
    await pickBucket('backups')
    await userEvent.click(screen.getByTestId('submit-access-key'))

    expect(await screen.findByText('Save your secret access key')).toBeInTheDocument()
    const created = mockAccessKeys().find((key) => key.name === 'web-app')
    expect(created).toMatchObject({ level: 'read-write', buckets: ['backups'] })
    const id = created?.access_key ?? ''

    const env = screen.getByLabelText('Environment variables') as HTMLTextAreaElement
    expect(env.value).toContain(`AWS_ACCESS_KEY_ID=${id}`)
    expect(env.value).toContain(`AWS_SECRET_ACCESS_KEY=mock-secret-${id}`)
    await waitFor(() =>
      expect(
        (screen.getByLabelText('Environment variables') as HTMLTextAreaElement).value,
      ).toContain('AWS_ENDPOINT_URL=http://s3.kumo.test:9000'),
    )

    // The secret is on screen, yet the store never holds it.
    expect(JSON.stringify(store.getState())).not.toContain('mock-secret-')

    const done = screen.getByTestId('secret-done')
    expect(done).toBeDisabled()
    await userEvent.keyboard('{Escape}')
    expect(screen.getByText('Save your secret access key')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('checkbox', { name: /I have saved the secret/ }))
    await userEvent.click(done)

    expect(await screen.findByText('web-app')).toBeInTheDocument()
    expect(document.body.textContent).not.toContain('mock-secret-')
    expect(JSON.stringify(store.getState())).not.toContain('mock-secret-')
  })

  it('checks the form before sending anything', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/access-keys/create' })

    await userEvent.type(await screen.findByLabelText('Name'), 'bad name')
    await userEvent.click(screen.getByTestId('submit-access-key'))

    expect(await screen.findByText(/Must be 1-64 letters/)).toBeInTheDocument()
    expect(screen.getByText('Choose at least one bucket.')).toBeInTheDocument()
    expect(mockAccessKeys()).toHaveLength(1)
  })

  it('shows a taken name from the API', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/access-keys/create' })

    await userEvent.type(await screen.findByLabelText('Name'), 'log-shipper')
    await pickBucket('backups')
    await userEvent.click(screen.getByTestId('submit-access-key'))

    expect(
      await screen.findByText(/an access key named "log-shipper" already exists/),
    ).toBeInTheDocument()
  })
})

describe('EditAccessKeyPage', () => {
  it('re-scopes a key without a new secret', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/access-keys/KUMOAAAABBBBCCCCDDDD/edit' })

    expect(await screen.findByDisplayValue('log-shipper')).toBeInTheDocument()
    await userEvent.click(screen.getByLabelText(/Read only/))
    await pickBucket('backups')
    await userEvent.click(screen.getByTestId('submit-access-key'))

    await waitFor(() =>
      expect(mockAccessKeys()[0]).toMatchObject({
        level: 'read-only',
        buckets: ['app-logs', 'backups'],
      }),
    )
    expect(screen.queryByText('Save your secret access key')).not.toBeInTheDocument()
  })
})
