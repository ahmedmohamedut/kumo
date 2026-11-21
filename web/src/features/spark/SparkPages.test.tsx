import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { navigationFor } from '@/components/layout/navigation'
import { CreateSparkClusterPage } from '@/features/spark/CreateSparkClusterPage'
import { SparkClusterDetailPage } from '@/features/spark/SparkClusterDetailPage'
import { SparkClustersListPage } from '@/features/spark/SparkClustersListPage'
import {
  bucketsProblem,
  canDelete,
  canDownloadConfig,
  canScale,
  nameProblem,
  SPARK_INDICATOR,
  submitExample,
  workerCountProblem,
} from '@/features/spark/state'
import {
  MOCK_VIEWER,
  mockSparkClusters,
  setMockInstanceTypes,
  setMockSpark,
  setMockSparkClusters,
} from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

const ETL = 'spk-0a1b2c3d4e5f60718'

function Pages() {
  return (
    <Routes>
      <Route path="/spark" element={<SparkClustersListPage />} />
      <Route path="/spark/create" element={<CreateSparkClusterPage />} />
      <Route path="/spark/:clusterId" element={<SparkClusterDetailPage />} />
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

describe('Spark state rules', () => {
  it('maps every state to an indicator, degraded as a warning', () => {
    expect(SPARK_INDICATOR.active.type).toBe('success')
    expect(SPARK_INDICATOR.degraded.type).toBe('warning')
    expect(SPARK_INDICATOR.failed.type).toBe('error')
    for (const state of ['creating', 'updating', 'deleting'] as const) {
      expect(SPARK_INDICATOR[state].type).toBe('in-progress')
    }
  })

  it('validates as the API does', () => {
    expect(nameProblem('etl')).toBeNull()
    expect(nameProblem('ETL')).not.toBeNull()
    expect(workerCountProblem(10)).toBeNull()
    expect(workerCountProblem(0)).not.toBeNull()
    expect(bucketsProblem(['a'], 'a')).toBeNull()
    expect(bucketsProblem(['a'], '')).toBeNull()
    expect(bucketsProblem([], '')).not.toBeNull()
    expect(bucketsProblem(['a'], 'b')).not.toBeNull()
  })

  it('allows each action in the states the API allows it', () => {
    expect(canScale('active') && canScale('degraded')).toBe(true)
    expect(canScale('creating')).toBe(false)
    expect(canDownloadConfig('creating')).toBe(false)
    expect(canDownloadConfig('active')).toBe(true)
    expect(canDelete('deleting')).toBe(false)
  })

  it('submits with the driver advertising its address', () => {
    const cluster = mockSparkClusters()[0]
    expect(cluster).toBeDefined()
    if (cluster !== undefined) {
      expect(submitExample(cluster)).toContain(`--properties-file ./kumo-${ETL}.conf`)
      expect(submitExample(cluster)).toContain('spark.driver.host=$(hostname -I')
    }
  })

  it('hides the Spark section only when the module is off', () => {
    const sections = (spark: boolean) =>
      navigationFor({ objectStorage: true, databases: true, spark }).map((e) =>
        'text' in e ? e.text : '',
      )
    expect(sections(true)).toContain('Spark')
    expect(sections(false)).not.toContain('Spark')
  })
})

describe('SparkClustersListPage', () => {
  it('lists the clusters with their state, workers and storage', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/spark' })

    expect(await screen.findByRole('link', { name: 'etl' })).toBeInTheDocument()
    const etl = rowOf('etl')
    expect(within(etl).getByText('Active')).toBeInTheDocument()
    expect(within(etl).getByText('2 / 2')).toBeInTheDocument()
    expect(within(etl).getByText('app-logs, backups')).toBeInTheDocument()
    expect(within(etl).getByText('spark://etl.default:7077')).toBeInTheDocument()
    expect(within(rowOf('adhoc')).getByText('Creating')).toBeInTheDocument()
  })

  it('explains how to turn the module on when it is off', async () => {
    setMockSpark({ enabled: false, versions: [], object_storage: false })
    await signInAs()
    renderWithProviders(<Pages />, { route: '/spark' })
    expect(await screen.findByText('Spark is turned off on this node')).toBeInTheDocument()
    expect(screen.getByText('KUMO_SPARK=on')).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('explains the image import when there is none', async () => {
    setMockSpark({ enabled: true, versions: [], object_storage: true })
    setMockSparkClusters([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/spark' })
    expect(await screen.findByText('No Spark image is available yet')).toBeInTheDocument()
    expect(screen.getByTestId('create-spark-cluster')).toBeDisabled()
  })

  it('keeps the write actions away from a viewer', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/spark' })
    await screen.findByRole('link', { name: 'etl' })
    expect(screen.getByTestId('create-spark-cluster')).toBeDisabled()
    expect(screen.getByTestId('delete-spark-cluster')).toBeDisabled()
  })

  it('deletes a cluster only once its name is typed, keeping its buckets', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/spark' })
    await screen.findByRole('link', { name: 'etl' })
    await userEvent.click(within(rowOf('etl')).getByRole('radio'))
    await userEvent.click(screen.getByTestId('delete-spark-cluster'))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/app-logs, backups/)).toBeInTheDocument()
    const confirm = screen.getByTestId('confirm-delete-spark-cluster')
    expect(confirm).toBeDisabled()
    await userEvent.type(screen.getByLabelText('Cluster name to confirm'), 'etl')
    await userEvent.click(confirm)
    await waitFor(() => {
      expect(mockSparkClusters().find((c) => c.id === ETL)?.state).toBe('deleting')
    })
  })
})

describe('CreateSparkClusterPage', () => {
  it('offers only the sizes a node needs', async () => {
    setMockInstanceTypes([
      { id: 'small', vcpus: 2, memory_mib: 2048, description: 'too small for a master' },
      { id: 'single', vcpus: 1, memory_mib: 4096, description: 'one vCPU' },
      { id: 'tiny', vcpus: 2, memory_mib: 1024, description: 'too little for anything' },
      { id: 'kumo.medium', vcpus: 2, memory_mib: 4096, description: 'fits both' },
    ])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/spark/create' })
    await screen.findByRole('heading', { name: 'Create Spark cluster', level: 1 })
    const optionsOf = async (label: string) => {
      await userEvent.click(screen.getByRole('button', { name: new RegExp(label) }))
      const listed = screen.getAllByRole('option').map((o) => o.textContent ?? '')
      await userEvent.keyboard('{Escape}')
      return listed
    }
    const master = await optionsOf('Master size')
    expect(master.some((o) => o.startsWith('kumo.medium'))).toBe(true)
    expect(master.some((o) => o.startsWith('small') || o.startsWith('single'))).toBe(false)
    const workers = await optionsOf('Worker size')
    expect(workers.some((o) => o.startsWith('single'))).toBe(true)
    expect(workers.some((o) => o.startsWith('tiny'))).toBe(false)
  })

  it('creates a cluster with object storage and lands on its page', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/spark/create' })
    await userEvent.type(await screen.findByLabelText('Cluster name'), 'reports')
    await userEvent.click(
      screen.getByRole('checkbox', { name: /Give the cluster access to buckets/ }),
    )
    await userEvent.click(screen.getByRole('button', { name: /Buckets/ }))
    await userEvent.click(await screen.findByRole('option', { name: /backups/ }))
    await userEvent.keyboard('{Escape}')
    await userEvent.click(screen.getByTestId('submit-create-spark-cluster'))

    expect(await screen.findByText('This cluster is being created')).toBeInTheDocument()
    const created = mockSparkClusters().find((c) => c.name === 'reports')
    expect(created?.object_storage).toBe(true)
    expect(created?.buckets).toEqual(['backups'])
    expect(created?.event_log_bucket).toBeNull()
    await waitFor(() => {
      expect(store.getState().notifications.items[0]?.header).toBe('Spark cluster creation started')
    })
  })

  it('refuses object storage with no bucket before the round trip', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/spark/create' })
    await userEvent.type(await screen.findByLabelText('Cluster name'), 'reports')
    await userEvent.click(
      screen.getByRole('checkbox', { name: /Give the cluster access to buckets/ }),
    )
    await userEvent.click(screen.getByTestId('submit-create-spark-cluster'))
    expect(
      await screen.findByText('Choose at least one bucket the cluster may use.'),
    ).toBeInTheDocument()
    expect(mockSparkClusters()).toHaveLength(2)
  })

  it('says object storage is off when the node has none', async () => {
    setMockSpark({ enabled: true, versions: ['4.3'], object_storage: false })
    await signInAs()
    renderWithProviders(<Pages />, { route: '/spark/create' })
    expect(await screen.findByText(/Object storage is off on this node/)).toBeInTheDocument()
    expect(screen.queryByTestId('spark-object-storage')).not.toBeInTheDocument()
  })
})

describe('SparkClusterDetailPage', () => {
  it('shows the cluster, its node group and its nodes', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: `/spark/${ETL}` })
    expect(await screen.findByRole('heading', { name: 'etl', level: 1 })).toBeInTheDocument()
    expect(screen.getByText(`s3a://app-logs/spark-events/${ETL}`)).toBeInTheDocument()
    await userEvent.click(screen.getByRole('tab', { name: 'Nodes (3)' }))
    const nodes = screen.getByRole('table', { name: 'Nodes' })
    expect(within(nodes).getByText('etl-master')).toBeInTheDocument()
    expect(within(nodes).getByText('Master')).toBeInTheDocument()
    expect(within(nodes).getAllByText('Alive')).toHaveLength(3)
  })

  it('downloads the client configuration for an admin', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: `/spark/${ETL}` })
    await screen.findByRole('heading', { name: 'etl', level: 1 })
    await userEvent.click(screen.getByRole('tab', { name: 'Connect' }))
    expect(screen.getByText('http://etl.default:18080')).toBeInTheDocument()
    expect(screen.getByText("Run spark-submit with the driver's address")).toBeInTheDocument()
    await userEvent.click(screen.getByTestId('download-spark-config'))
    expect(await screen.findByText('Downloaded.')).toBeInTheDocument()
    await waitFor(() => {
      expect(store.getState().notifications.items[0]?.header).toBe(
        'Client configuration downloaded',
      )
    })
  })

  it('offers a viewer no client configuration and no writes', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: `/spark/${ETL}` })
    await screen.findByRole('heading', { name: 'etl', level: 1 })
    expect(screen.getByTestId('delete-spark-cluster')).toBeDisabled()
    expect(screen.getByTestId('resize-spark-node-group')).toBeDisabled()
    await userEvent.click(screen.getByRole('tab', { name: 'Connect' }))
    expect(screen.queryByTestId('download-spark-config')).not.toBeInTheDocument()
    expect(screen.getByText(/only an administrator can/)).toBeInTheDocument()
  })

  it('resizes the node group, and warns about the decommission when shrinking', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: `/spark/${ETL}` })
    await screen.findByRole('heading', { name: 'etl', level: 1 })
    await userEvent.click(screen.getByTestId('resize-spark-node-group'))
    const nodes = await screen.findByLabelText('Nodes')
    await userEvent.clear(nodes)
    await userEvent.type(nodes, '1')
    expect(screen.getByText('The newest workers are decommissioned first')).toBeInTheDocument()
    await userEvent.click(screen.getByTestId('confirm-resize'))
    await waitFor(() => {
      const etl = mockSparkClusters().find((c) => c.id === ETL)
      expect(etl?.state).toBe('updating')
      expect(etl?.node_groups[0]?.desired_size).toBe(1)
    })
    await waitFor(() => {
      expect(store.getState().notifications.items[0]?.header).toBe('Node group resizing')
    })
  })

  it('explains a degraded cluster with its reason', async () => {
    setMockSparkClusters(
      mockSparkClusters().map((c) =>
        c.id === ETL
          ? { ...c, state: 'degraded', state_reason: 'workers not registered: etl-default-2' }
          : c,
      ),
    )
    await signInAs()
    renderWithProviders(<Pages />, { route: `/spark/${ETL}` })
    expect(await screen.findByText('This cluster is degraded')).toBeInTheDocument()
    expect(screen.getByText(/etl-default-2/)).toBeInTheDocument()
  })
})
