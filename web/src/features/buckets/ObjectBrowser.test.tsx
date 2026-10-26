import { describe, expect, it, vi } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { BucketDetailPage } from '@/features/buckets/BucketDetailPage'
import { isPlainHTTP } from '@/features/buckets/transfer'
import { MOCK_S3_ENDPOINT, MOCK_VIEWER, mockBuckets, setMockObjectPageSize } from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

const downloads = vi.hoisted(() => ({ urls: [] as string[] }))

vi.mock('@/features/buckets/transfer', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/features/buckets/transfer')>()
  return {
    ...actual,
    startDownload: (url: string) => {
      downloads.urls.push(url)
    },
  }
})

function Pages() {
  return (
    <Routes>
      <Route path="/buckets/:bucketName" element={<BucketDetailPage />} />
    </Routes>
  )
}

function objectsIn(bucket: string): string[] {
  return mockBuckets()
    .find((entry) => entry.bucket.name === bucket)
    ?.objects.map((object) => object.key)
    .sort() as string[]
}

function rowOf(name: string): HTMLElement {
  const cell = screen.getAllByText(name).find((element) => element.closest('tr') !== null)
  const row = cell?.closest('tr')
  if (row === null || row === undefined) {
    throw new Error(`no row for ${name}`)
  }
  return row
}

describe('ObjectBrowser', () => {
  it('lists the top of a bucket, then opens a folder and comes back', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/app-logs' })

    expect(await screen.findByRole('button', { name: 'Open folder logs/' })).toBeInTheDocument()
    expect(within(rowOf('README.md')).getByText('2.0 KiB')).toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Open folder logs/' }))
    expect(await screen.findByText('2026-01-05.log')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Open folder archive/' })).toBeInTheDocument()
    expect(screen.queryByText('README.md')).not.toBeInTheDocument()

    const path = screen.getByRole('navigation', { name: 'Folder path' })
    await userEvent.click(within(path).getAllByText('app-logs')[0] as HTMLElement)
    expect(await screen.findByText('README.md')).toBeInTheDocument()
  })

  it('opens the folder named in the URL', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/app-logs?prefix=logs/archive/' })

    expect(await screen.findByText('2025.tar.gz')).toBeInTheDocument()
  })

  it('pages through a long folder', async () => {
    setMockObjectPageSize(1)
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/app-logs?prefix=logs/' })

    expect(await screen.findByText('2026-01-04.log')).toBeInTheDocument()
    expect(screen.queryByText('2026-01-05.log')).not.toBeInTheDocument()

    await userEvent.click(screen.getByRole('button', { name: 'Next page' }))
    expect(await screen.findByText('2026-01-05.log')).toBeInTheDocument()
    expect(screen.queryByText('2026-01-04.log')).not.toBeInTheDocument()
  })

  it('uploads files straight to the object store', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/app-logs?prefix=logs/' })
    await screen.findByText('2026-01-04.log')

    const files = [
      new File(['hello'], 'hello.txt', { type: 'text/plain' }),
      new File([new Uint8Array(10)], 'blob.bin'),
    ]
    await userEvent.upload(screen.getByTestId('upload-input'), files)

    await waitFor(() =>
      expect(objectsIn('app-logs')).toEqual(
        expect.arrayContaining(['logs/hello.txt', 'logs/blob.bin']),
      ),
    )
    const stored = mockBuckets()[0]?.objects.find((object) => object.key === 'logs/hello.txt')
    expect(stored).toMatchObject({ size_bytes: 5, content_type: 'text/plain' })
    expect(
      mockBuckets()[0]?.objects.find((object) => object.key === 'logs/blob.bin')?.content_type,
    ).toBe('application/octet-stream')

    expect(screen.getByText('Uploads')).toBeInTheDocument()
    // The folder is listed again once the uploads finish.
    await waitFor(() => expect(screen.getAllByText('hello.txt').length).toBeGreaterThan(1))
  })

  it('downloads the selected object through a presigned URL', async () => {
    downloads.urls = []
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/buckets/app-logs' })

    await screen.findByText('README.md')
    const download = screen.getByRole('button', { name: 'Download' })
    expect(download).toBeDisabled()
    await userEvent.click(within(rowOf('README.md')).getByRole('radio'))
    await userEvent.click(download)

    await waitFor(() =>
      expect(downloads.urls).toEqual([
        `${MOCK_S3_ENDPOINT}/app-logs/README.md?X-Amz-Signature=mock`,
      ]),
    )
  })

  it('creates a folder inside the one being browsed', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/app-logs?prefix=logs/' })
    await screen.findByText('2026-01-04.log')

    await userEvent.click(screen.getByRole('button', { name: 'Create folder' }))
    await userEvent.click(screen.getByTestId('submit-create-folder'))
    expect(await screen.findByText('Enter a folder name without slashes.')).toBeInTheDocument()

    await userEvent.type(screen.getByLabelText('Folder name'), '2027')
    await userEvent.click(screen.getByTestId('submit-create-folder'))

    expect(await screen.findByRole('button', { name: 'Open folder 2027/' })).toBeInTheDocument()
    expect(objectsIn('app-logs')).toContain('logs/2027/')
  })

  it('deletes the selected objects after confirmation', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/app-logs?prefix=logs/' })
    await screen.findByText('2026-01-04.log')

    await userEvent.click(within(rowOf('2026-01-04.log')).getByRole('checkbox'))
    await userEvent.click(within(rowOf('2026-01-05.log')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-objects'))

    const list = await screen.findByRole('list', { name: 'Items to delete' })
    expect(within(list).getAllByRole('listitem')).toHaveLength(2)
    await userEvent.click(screen.getByTestId('confirm-delete-objects'))

    await waitFor(() =>
      expect(objectsIn('app-logs')).not.toEqual(expect.arrayContaining(['logs/2026-01-04.log'])),
    )
    expect(objectsIn('app-logs')).not.toContain('logs/2026-01-05.log')
    await waitFor(() => expect(screen.queryByText('2026-01-04.log')).not.toBeInTheDocument())
  })

  it('says a folder delete keeps what is inside', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/app-logs' })
    await screen.findByText('README.md')

    await userEvent.click(within(rowOf('logs/')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-objects'))
    expect(
      await screen.findByText(/Deleting a folder removes only the folder itself/),
    ).toBeInTheDocument()
  })

  it('keeps a viewer to browsing and downloading', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/buckets/app-logs' })
    await screen.findByText('README.md')

    expect(screen.getByRole('button', { name: 'Upload' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Create folder' })).toBeDisabled()
    expect(screen.getByTestId('delete-objects')).toBeDisabled()
    expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  })

  it('shows an empty bucket', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/backups' })

    expect(await screen.findByText('This bucket is empty')).toBeInTheDocument()
  })
})

describe('isPlainHTTP', () => {
  it.each([
    [{ protocol: 'http:', hostname: 'kumo.lan' }, true],
    [{ protocol: 'https:', hostname: 'kumo.lan' }, false],
    [{ protocol: 'http:', hostname: 'localhost' }, false],
    [{ protocol: 'http:', hostname: '127.0.0.1' }, false],
  ])('%j is %s', (location, want) => {
    expect(isPlainHTTP(location)).toBe(want)
  })
})
