import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { BucketsListPage } from '@/features/buckets/BucketsListPage'
import { CreateBucketPage } from '@/features/buckets/CreateBucketPage'
import { BucketDetailPage } from '@/features/buckets/BucketDetailPage'
import { bucketNameProblem } from '@/features/buckets/state'
import {
  MOCK_VIEWER,
  mockBuckets,
  mockTasks,
  setMockBuckets,
  setMockObjectStorage,
} from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

function Pages() {
  return (
    <Routes>
      <Route path="/buckets" element={<BucketsListPage />} />
      <Route path="/buckets/create" element={<CreateBucketPage />} />
      <Route path="/buckets/:bucketName" element={<BucketDetailPage />} />
    </Routes>
  )
}

function rowOf(name: string): HTMLElement {
  const row = screen.getByRole('link', { name }).closest('tr')
  if (row === null) {
    throw new Error(`no table row for ${name}`)
  }
  return row
}

async function select(name: string) {
  await userEvent.click(within(rowOf(name)).getByRole('radio'))
}

function bucketOf(name: string) {
  return mockBuckets().find((entry) => entry.bucket.name === name)
}

describe('BucketsListPage', () => {
  it('lists the buckets with their creator', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets' })

    expect(await screen.findByRole('link', { name: 'app-logs' })).toBeInTheDocument()
    expect(within(rowOf('app-logs')).getByText('ada')).toBeInTheDocument()
    expect(within(rowOf('app-logs')).getByText('Available')).toBeInTheDocument()
    expect(within(rowOf('app-logs')).getByText('1 tag')).toBeInTheDocument()
    // A bucket made with another S3 client has no creator Kumo knows of.
    expect(within(rowOf('backups')).getByText('Outside Kumo')).toBeInTheDocument()
  })

  it('explains how to turn object storage on when it is off', async () => {
    setMockObjectStorage({ backend: 'none', ok: false })
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets' })

    expect(
      await screen.findByText('Object storage is not configured on this node'),
    ).toBeInTheDocument()
    expect(screen.getByText('KUMO_OBJECTSTORE=rustfs')).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('warns when the object storage server is not answering', async () => {
    setMockObjectStorage({ backend: 'rustfs', ok: false })
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets' })

    expect(
      await screen.findByText('The object storage server is not answering'),
    ).toBeInTheDocument()
    expect(await screen.findByText('The buckets could not be loaded')).toBeInTheDocument()
  })

  it('keeps the write actions away from a viewer', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/buckets' })

    await screen.findByRole('link', { name: 'app-logs' })
    expect(screen.getByRole('button', { name: 'Create bucket' })).toBeDisabled()
    expect(screen.getByTestId('delete-bucket')).toBeDisabled()
    expect(screen.queryByRole('radio')).not.toBeInTheDocument()
  })

  it('shows the empty state with a create button', async () => {
    setMockBuckets([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets' })

    expect(await screen.findByText('No buckets')).toBeInTheDocument()
    await userEvent.click(
      screen.getAllByRole('button', { name: 'Create bucket' })[1] as HTMLElement,
    )
    expect(await screen.findByLabelText('Bucket name')).toBeInTheDocument()
  })

  it('deletes an empty bucket', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets' })

    await screen.findByRole('link', { name: 'backups' })
    await select('backups')
    await userEvent.click(screen.getByTestId('delete-bucket'))
    await userEvent.click(await screen.findByTestId('confirm-delete-bucket'))

    await waitFor(() => expect(bucketOf('backups')).toBeUndefined())
    await waitFor(() =>
      expect(screen.queryByRole('link', { name: 'backups' })).not.toBeInTheDocument(),
    )
  })

  it('offers the forced delete when the bucket is not empty, and asks for its name', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets' })

    await screen.findByRole('link', { name: 'app-logs' })
    await select('app-logs')
    await userEvent.click(screen.getByTestId('delete-bucket'))
    await userEvent.click(await screen.findByTestId('confirm-delete-bucket'))

    expect(await screen.findByText('This bucket is not empty')).toBeInTheDocument()
    expect(bucketOf('app-logs')?.bucket.state).toBe('available')

    await userEvent.click(screen.getByRole('checkbox', { name: /Delete all contents/ }))
    const confirm = screen.getByTestId('confirm-delete-bucket')
    expect(confirm).toBeDisabled()
    expect(confirm).toHaveTextContent('Delete bucket and contents')

    await userEvent.type(screen.getByLabelText('Bucket name to confirm'), 'app-log')
    expect(confirm).toBeDisabled()
    await userEvent.type(screen.getByLabelText('Bucket name to confirm'), 's')
    expect(confirm).toBeEnabled()
    await userEvent.click(confirm)

    await waitFor(() => expect(bucketOf('app-logs')?.bucket.state).toBe('deleting'))
    expect(mockTasks()[0]).toMatchObject({ type: 'bucket.delete', resource_id: 'bucket/app-logs' })
    expect(await screen.findByText('Deleting')).toBeInTheDocument()
  })
})

describe('CreateBucketPage', () => {
  it('creates a bucket with versioning, then opens it', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/create' })

    await userEvent.type(await screen.findByLabelText('Bucket name'), 'media-uploads')
    await userEvent.click(screen.getByText('Enable versioning'))
    await userEvent.click(screen.getByTestId('submit-create-bucket'))

    expect(await screen.findByRole('heading', { name: 'media-uploads' })).toBeInTheDocument()
    expect(await screen.findByTestId('bucket-versioning')).toHaveTextContent('Enabled')
    expect(bucketOf('media-uploads')?.versioning).toBe('enabled')
  })

  it('refuses a bad name without a request', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/create' })

    await userEvent.type(await screen.findByLabelText('Bucket name'), 'Bad_Name')
    await userEvent.click(screen.getByTestId('submit-create-bucket'))

    expect(await screen.findByText(/Must be 3-63 lowercase letters/)).toBeInTheDocument()
    expect(bucketOf('Bad_Name')).toBeUndefined()
  })

  it('shows a taken name from the API on the field', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/create' })

    await userEvent.type(await screen.findByLabelText('Bucket name'), 'backups')
    await userEvent.click(screen.getByTestId('submit-create-bucket'))

    expect(await screen.findByText('The bucket was not created')).toBeInTheDocument()
    expect(screen.getByText(/a bucket named "backups" already exists/)).toBeInTheDocument()
  })
})

describe('BucketDetailPage', () => {
  it('shows the details and how to connect', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/app-logs' })

    expect(await screen.findByTestId('bucket-versioning')).toHaveTextContent('Enabled')
    expect(screen.getByText('ada')).toBeInTheDocument()
    expect(screen.getByText('us-east-1')).toBeInTheDocument()
    expect(screen.getByText('http://s3.kumo.test:9000')).toBeInTheDocument()
    expect(
      screen.getByText('aws --endpoint-url http://s3.kumo.test:9000 s3 ls s3://app-logs/'),
    ).toBeInTheDocument()
    expect(screen.getByText('prod')).toBeInTheDocument()
  })

  it('suspends and re-enables versioning', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/app-logs' })

    await screen.findByTestId('bucket-versioning')
    await userEvent.click(screen.getByRole('button', { name: 'Suspend' }))
    await waitFor(() =>
      expect(screen.getByTestId('bucket-versioning')).toHaveTextContent('Suspended'),
    )
    await userEvent.click(screen.getByRole('button', { name: 'Enable' }))
    await waitFor(() =>
      expect(screen.getByTestId('bucket-versioning')).toHaveTextContent('Enabled'),
    )
  })

  it('keeps a viewer read-only', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/buckets/app-logs' })

    await screen.findByTestId('bucket-versioning')
    expect(screen.getByRole('button', { name: 'Suspend' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Edit tags' })).toBeDisabled()
    expect(screen.getByTestId('delete-bucket')).toBeDisabled()
  })

  it('says a bucket that does not exist is gone', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/buckets/nope' })

    expect(await screen.findByText('This bucket does not exist')).toBeInTheDocument()
  })
})

describe('bucketNameProblem', () => {
  it.each([
    ['', 'Enter a name for the bucket.'],
    ['ab', expect.stringMatching(/^Must be/)],
    ['App', expect.stringMatching(/^Must be/)],
    ['a..b', expect.stringMatching(/^Must be/)],
    ['10.0.0.1', expect.stringMatching(/^Must be/)],
    ['-abc', expect.stringMatching(/^Must be/)],
  ])('refuses %j', (name, want) => {
    expect(bucketNameProblem(name)).toEqual(want)
  })

  it.each(['app-logs', 'a.b.c', 'x1y'])('accepts %j', (name) => {
    expect(bucketNameProblem(name)).toBeNull()
  })
})
