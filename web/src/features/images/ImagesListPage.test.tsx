import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { ImagesListPage } from '@/features/images/ImagesListPage'
import { addMockImageReference, mockImages, setMockImages } from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

function Pages() {
  return (
    <Routes>
      <Route path="/images" element={<ImagesListPage />} />
      <Route path="/images/import" element={<p>Import image</p>} />
      <Route path="/images/:imageId" element={<p>Image detail</p>} />
    </Routes>
  )
}

/** rowOf finds the table row holding an image name. */
function rowOf(name: string): HTMLElement {
  const cell = screen.getByRole('link', { name })
  const row = cell.closest('tr')
  if (row === null) {
    throw new Error(`no table row for ${name}`)
  }
  return row
}

describe('ImagesListPage', () => {
  it('lists the images newest first with their state and size', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images' })

    expect(await screen.findByRole('link', { name: 'alpine-3.20' })).toBeInTheDocument()

    const alpine = rowOf('alpine-3.20')
    expect(within(alpine).getByText('Available')).toBeInTheDocument()
    expect(within(alpine).getByText('64.0 MiB')).toBeInTheDocument()
    expect(within(alpine).getByText('Alpine 3.20')).toBeInTheDocument()
    expect(within(alpine).getByText('x86_64')).toBeInTheDocument()

    const names = screen.getAllByRole('link').map((link) => link.textContent)
    expect(names).toEqual(['alpine-3.20', 'rocky-9-arm64', 'debian-12-generic', 'ubuntu-22.04'])
  })

  it('shows a progress bar only while a download is under way', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images' })

    const downloading = await screen.findByRole('link', { name: 'debian-12-generic' })
    const row = downloading.closest('tr')
    if (row === null) {
      throw new Error('no row for debian-12-generic')
    }
    expect(within(row).getByText('Downloading')).toBeInTheDocument()
    expect(within(row).getByRole('progressbar', { name: /Download progress/ })).toBeInTheDocument()
    expect(within(row).getAllByText('42%').length).toBeGreaterThan(0)

    // An available image is finished: no bar claiming to be at nothing.
    expect(within(rowOf('ubuntu-22.04')).queryByRole('progressbar')).not.toBeInTheDocument()
  })

  it('shows the reason an import failed, and nothing for a healthy image', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images' })

    expect(await screen.findByRole('link', { name: 'rocky-9-arm64' })).toBeInTheDocument()
    const failed = rowOf('rocky-9-arm64')
    expect(within(failed).getByText('Failed')).toBeInTheDocument()
    expect(
      within(failed).getByText('the source returned 403 for the requested image'),
    ).toBeInTheDocument()

    const healthy = rowOf('ubuntu-22.04')
    expect(within(healthy).queryByText(/failed/i)).not.toBeInTheDocument()
  })

  it('filters on the server and keeps the filter in the URL', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images' })

    await screen.findByRole('link', { name: 'ubuntu-22.04' })
    await userEvent.type(screen.getByLabelText('Filter images'), 'debian')

    await waitFor(() =>
      expect(screen.queryByRole('link', { name: 'ubuntu-22.04' })).not.toBeInTheDocument(),
    )
    expect(screen.getByRole('link', { name: 'debian-12-generic' })).toBeInTheDocument()
  })

  it('offers both ways to add an image when there are none', async () => {
    setMockImages([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images' })

    expect(await screen.findByRole('heading', { name: 'No images' })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Import your first image' }))
    expect(await screen.findByText('Import image')).toBeInTheDocument()
  })

  it('sends the operator to the import form', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images' })

    await screen.findByRole('link', { name: 'ubuntu-22.04' })
    await userEvent.click(screen.getByTestId('import-image'))

    expect(await screen.findByText('Import image')).toBeInTheDocument()
  })

  it('asks before deleting, and only queues the delete once confirmed', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images' })

    await screen.findByRole('link', { name: 'debian-12-generic' })
    expect(screen.getByTestId('delete-images')).toBeDisabled()

    await userEvent.click(within(rowOf('debian-12-generic')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-images'))

    const dialog = await screen.findByRole('dialog')
    expect(
      within(dialog).getByRole('heading', { name: 'Delete debian-12-generic?' }),
    ).toBeInTheDocument()
    // The confirmation is not a side effect: the image is still there.
    expect(mockImages().map((image) => image.name)).toContain('debian-12-generic')

    await userEvent.click(screen.getByTestId('confirm-delete'))

    // A delete is a task, so the image stays visible in `deleting` until the
    // worker removes the volume.
    await waitFor(() =>
      expect(within(rowOf('debian-12-generic')).getByText('Deleting')).toBeInTheDocument(),
    )
    expect(mockImages().find((image) => image.name === 'debian-12-generic')?.state).toBe('deleting')
  })

  it('leaves the image alone when the confirmation is cancelled', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images' })

    await screen.findByRole('link', { name: 'debian-12-generic' })
    await userEvent.click(within(rowOf('debian-12-generic')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-images'))

    const dialog = await screen.findByRole('dialog')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(mockImages().find((image) => image.name === 'debian-12-generic')?.state).toBe(
      'downloading',
    )
  })

  it('names the instance that blocks the delete when the API answers 409', async () => {
    // alpine-3.20 is launched-from by i-0a1b2c3d4e5f60718 in the fixtures.
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/images' })

    await screen.findByRole('link', { name: 'alpine-3.20' })
    await userEvent.click(within(rowOf('alpine-3.20')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-images'))
    await userEvent.click(screen.getByTestId('confirm-delete'))

    expect(await screen.findByRole('link', { name: 'alpine-3.20' })).toBeInTheDocument()
    const notifications = store.getState().notifications.items
    expect(notifications).toHaveLength(1)
    expect(notifications[0]?.type).toBe('error')
    expect(notifications[0]?.content).toContain('alpine-3.20')
    expect(notifications[0]?.content).toContain('i-0a1b2c3d4e5f60718')
  })

  it('re-reads an image an instance started using since the last poll', async () => {
    addMockImageReference('img-0b2c3d4e5f6071890', 'i-0fffffffffffffff')
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/images' })

    await screen.findByRole('link', { name: 'debian-12-generic' })
    await userEvent.click(within(rowOf('debian-12-generic')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-images'))
    await userEvent.click(screen.getByTestId('confirm-delete'))

    expect(await screen.findByRole('link', { name: 'debian-12-generic' })).toBeInTheDocument()
    const notifications = store.getState().notifications.items
    expect(notifications[0]?.type).toBe('error')
    expect(notifications[0]?.content).toContain('i-0fffffffffffffff')
  })

  it('refreshes the table on demand', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/images' })

    await screen.findByRole('link', { name: 'ubuntu-22.04' })
    await userEvent.click(screen.getByRole('button', { name: 'Refresh images' }))

    expect(await screen.findByRole('link', { name: 'ubuntu-22.04' })).toBeInTheDocument()
  })
})
