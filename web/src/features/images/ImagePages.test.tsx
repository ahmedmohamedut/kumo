import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { ImportImagePage } from '@/features/images/ImportImagePage'
import { ImageDetailPage } from '@/features/images/ImageDetailPage'
import { MOCK_VIEWER, mockImages, mockTasks } from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

function Pages() {
  return (
    <Routes>
      <Route path="/images" element={<p>Images</p>} />
      <Route path="/images/import" element={<ImportImagePage />} />
      <Route path="/images/:imageId" element={<ImageDetailPage />} />
    </Routes>
  )
}

/** pickSource switches the form between a URL and the catalog. */
async function pickSource(option: RegExp) {
  // Cloudscape names the trigger after both the FormField label and its own
  // content, so the match is partial.
  await userEvent.click(screen.getByRole('button', { name: /Public URL/ }))
  await userEvent.click(await screen.findByRole('option', { name: option }))
}

describe('ImportImagePage', () => {
  it('imports from a URL and hands the operator over to the new image', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/import' })

    await userEvent.type(screen.getByLabelText('Name'), 'ubuntu-24.04')
    await userEvent.type(
      screen.getByLabelText('Image URL'),
      'https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img',
    )
    await userEvent.click(screen.getByTestId('submit-import'))

    // The `202` answers with the image in `pending`, so the page goes to it
    // instead of waiting on a download that may take minutes.
    expect(
      await screen.findByRole('heading', { level: 1, name: 'ubuntu-24.04' }),
    ).toBeInTheDocument()
    expect(screen.queryByTestId('submit-import')).not.toBeInTheDocument()

    const created = mockImages().find((image) => image.name === 'ubuntu-24.04')
    expect(created?.state).toBe('pending')
    expect(created?.task_id).toBeDefined()
    // `os_family` defaults to linux in the form, so a minimal import is valid.
    expect(created?.os_family).toBe('linux')
  })

  it('fills the form from a catalog entry', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/import' })

    await pickSource(/Well-known image/)
    await userEvent.click(screen.getByRole('button', { name: /Choose an image/ }))
    await userEvent.click(await screen.findByRole('option', { name: /Debian 12/ }))

    // The picker shows what was picked instead of going back to blank.
    expect(screen.getByTestId('import-catalog')).toHaveTextContent(/Debian 12/)
    expect(screen.queryByRole('button', { name: /Choose an image/ })).not.toBeInTheDocument()

    // The catalog knows the URL, the OS and the disk size; asking the operator
    // to retype them only invites a typo that shows up at boot. The name comes
    // from the catalog id, because a picker label cannot be an image name.
    expect(screen.getByLabelText('Name')).toHaveValue('debian-12')
    expect(screen.getByLabelText('Operating system family')).toHaveValue('debian')
    expect(screen.getByLabelText('Operating system version')).toHaveValue('12')
    expect(screen.getByLabelText('Default user')).toHaveValue('debian')
    expect(screen.getByLabelText('Minimum disk size')).toHaveValue('8')

    await userEvent.click(screen.getByTestId('submit-import'))

    await waitFor(() => expect(mockImages().some((image) => image.name === 'debian-12')).toBe(true))
    expect(mockImages().find((image) => image.name === 'debian-12')?.source_url).toContain(
      'cloud.debian.org',
    )
  })

  it('refuses a bad URL and a bad disk size without asking the node', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/import' })

    const before = mockImages().length
    await userEvent.type(screen.getByLabelText('Name'), 'broken')
    await userEvent.type(screen.getByLabelText('Image URL'), 'ftp://example.invalid/image.img')
    await userEvent.type(screen.getByLabelText('Minimum disk size'), '0')
    await userEvent.click(screen.getByTestId('submit-import'))

    expect(await screen.findByText(/must start with http/)).toBeInTheDocument()
    expect(screen.getByText(/between 1 and 65536/)).toBeInTheDocument()
    // Nothing was sent, so nothing on the node moved.
    expect(mockImages()).toHaveLength(before)
  })

  it('asks for a name', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/import' })

    await userEvent.type(screen.getByLabelText('Image URL'), 'https://example.com/image.qcow2')
    await userEvent.click(screen.getByTestId('submit-import'))

    expect(await screen.findByText('Enter a name for the image.')).toBeInTheDocument()
  })

  it('shows the reason the API refused, on the field at fault', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/import' })

    await userEvent.type(screen.getByLabelText('Name'), 'ubuntu-22.04')
    await userEvent.type(
      screen.getByLabelText('Image URL'),
      'https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img',
    )
    await userEvent.click(screen.getByTestId('submit-import'))

    expect(await screen.findByText(/already exists/)).toBeInTheDocument()
  })

  it('sends a checksum the operator pasted, and refuses one of the wrong length', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/import' })

    await userEvent.type(screen.getByLabelText('Name'), 'checked-image')
    await userEvent.type(screen.getByLabelText('Image URL'), 'https://example.com/image.qcow2')
    await userEvent.type(screen.getByLabelText('Checksum value'), 'abc123')
    await userEvent.click(screen.getByTestId('submit-import'))

    // 6 hex characters is not a SHA-256 digest, and it is caught here rather
    // than after downloading the whole file.
    expect(await screen.findByText(/hex characters, this one is 6/)).toBeInTheDocument()
    expect(mockImages().some((image) => image.name === 'checked-image')).toBe(false)

    await userEvent.clear(screen.getByLabelText('Checksum value'))
    await userEvent.type(screen.getByLabelText('Checksum value'), 'a'.repeat(64))
    await userEvent.click(screen.getByTestId('submit-import'))

    await waitFor(() =>
      expect(mockImages().some((image) => image.name === 'checked-image')).toBe(true),
    )
    expect(mockImages().find((image) => image.name === 'checked-image')?.checksum).toEqual({
      algorithm: 'sha256',
      value: 'a'.repeat(64),
    })
  })

  it('refuses to import as a viewer', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/images/import' })

    await userEvent.type(screen.getByLabelText('Name'), 'ubuntu-24.04')
    await userEvent.type(
      screen.getByLabelText('Image URL'),
      'https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img',
    )
    await userEvent.click(screen.getByTestId('submit-import'))

    expect(await screen.findByText('this action requires the admin role')).toBeInTheDocument()
    expect(mockImages().some((image) => image.name === 'ubuntu-24.04')).toBe(false)
  })
})

describe('ImageDetailPage', () => {
  it('shows the image, its state and its details', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/img-0a1b2c3d4e5f60718' })

    expect(await screen.findByRole('heading', { name: 'ubuntu-22.04' })).toBeInTheDocument()
    expect(screen.getByText('Available')).toBeInTheDocument()
    expect(screen.getByText('Ubuntu 22.04')).toBeInTheDocument()
    expect(screen.getByText('qcow2')).toBeInTheDocument()
    // 687997184 bytes, the way an operator reads a disk size.
    expect(screen.getByText('656.1 MiB')).toBeInTheDocument()
    expect(
      screen.getByText(
        'https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img',
      ),
    ).toBeInTheDocument()
    // The one tag the fixture carries.
    expect(screen.getByText('cloud')).toBeInTheDocument()
    expect(screen.getByText('8 GiB')).toBeInTheDocument()
  })

  it('lists only the tasks that worked on this image', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/img-0c2d3e4f5a6071819' })

    expect(await screen.findByRole('heading', { name: 'rocky-9-arm64' })).toBeInTheDocument()
    // The task list is a second query, so it is waited for rather than assumed to
    // have landed with the image.
    expect(await screen.findByText('image.import')).toBeInTheDocument()
    // One "Failed" for the image state, one for the task that failed: the image
    // says what happened, the task says which run it happened in.
    expect(await screen.findAllByText('Failed')).toHaveLength(2)
    expect(screen.getAllByText(/403/).length).toBeGreaterThan(0)
    expect(mockTasks().filter((task) => task.resource_id === 'img-0c2d3e4f5a6071819')).toHaveLength(
      1,
    )
  })

  it('offers a retry only for a failed import, and queues a new task for it', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/images/img-0c2d3e4f5a6071819' })

    await screen.findByRole('heading', { name: 'rocky-9-arm64' })
    // One in the alert, one in the header; both are the same request.
    await userEvent.click(screen.getAllByTestId('retry-image')[0] as HTMLElement)

    await waitFor(() => {
      expect(mockImages().find((image) => image.id === 'img-0c2d3e4f5a6071819')?.state).toBe(
        'pending',
      )
    })
    expect(store.getState().notifications.items[0]?.header).toBe('Retry started')
    // A retry is a new task, not the old one resurrected.
    expect(mockTasks().filter((task) => task.resource_id === 'img-0c2d3e4f5a6071819')).toHaveLength(
      2,
    )
  })

  it('has no retry for an image that imported fine', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/img-0a1b2c3d4e5f60718' })

    await screen.findByRole('heading', { name: 'ubuntu-22.04' })
    expect(screen.queryByTestId('retry-image')).not.toBeInTheDocument()
  })

  it('asks before deleting, then queues the delete and returns to the list', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/img-0b2c3d4e5f6071890' })

    await screen.findByRole('heading', { name: 'debian-12-generic' })
    await userEvent.click(screen.getByTestId('delete-image'))

    const dialog = await screen.findByRole('dialog')
    expect(
      within(dialog).getByRole('heading', { name: 'Delete debian-12-generic?' }),
    ).toBeInTheDocument()
    // The confirmation is not a side effect: the image is untouched so far.
    expect(mockImages().find((image) => image.id === 'img-0b2c3d4e5f6071890')?.state).toBe(
      'downloading',
    )

    await userEvent.click(screen.getByTestId('confirm-delete-image'))

    expect(await screen.findByText('Images')).toBeInTheDocument()
    expect(mockImages().find((image) => image.id === 'img-0b2c3d4e5f6071890')?.state).toBe(
      'deleting',
    )
  })

  it('stays on the page and explains a refused delete', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/img-0d3e4f5a6071829' })

    await screen.findByRole('heading', { name: 'alpine-3.20' })
    await userEvent.click(screen.getByTestId('delete-image'))
    await userEvent.click(await screen.findByTestId('confirm-delete-image'))

    // A 409 names the instance that blocks the delete. Navigating away would
    // throw that answer away, which is why the page stays.
    expect(await screen.findByText(/still referenced by i-0a1b2c3d4e5f60718/)).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'alpine-3.20' })).toBeInTheDocument()
  })

  it('reports an image that does not exist', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/img-fffffffffffffffff' })

    expect(await screen.findByText('This image could not be loaded')).toBeInTheDocument()
    expect(screen.getByText('no image "img-fffffffffffffffff"')).toBeInTheDocument()
  })

  it('follows a download that is under way', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images/img-0b2c3d4e5f6071890' })

    expect(await screen.findByText('Downloading')).toBeInTheDocument()
    // A download in flight shows its percentage.
    expect(screen.getAllByText('42%').length).toBeGreaterThan(0)
  })
})
