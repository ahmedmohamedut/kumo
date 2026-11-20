import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { ClusterDetailPage } from '@/features/kubernetes/ClusterDetailPage'
import { ClustersListPage } from '@/features/kubernetes/ClustersListPage'
import { CreateClusterPage } from '@/features/kubernetes/CreateClusterPage'
import {
  canDelete,
  canDownloadKubeconfig,
  canScale,
  CLUSTER_INDICATOR,
  nameProblem,
  nodeCountProblem,
} from '@/features/kubernetes/state'
import { navigationFor } from '@/components/layout/navigation'
import {
  MOCK_VIEWER,
  mockClusters,
  setMockClusters,
  setMockInstanceTypes,
  setMockKubernetes,
} from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

const ORDERS = 'eks-0a1b2c3d4e5f60718'

function Pages() {
  return (
    <Routes>
      <Route path="/kubernetes" element={<ClustersListPage />} />
      <Route path="/kubernetes/create" element={<CreateClusterPage />} />
      <Route path="/kubernetes/:clusterId" element={<ClusterDetailPage />} />
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

describe('cluster state rules', () => {
  it('maps every state to an indicator, degraded as a warning', () => {
    expect(CLUSTER_INDICATOR.active.type).toBe('success')
    expect(CLUSTER_INDICATOR.degraded.type).toBe('warning')
    expect(CLUSTER_INDICATOR.failed.type).toBe('error')
    for (const state of ['creating', 'updating', 'deleting'] as const) {
      expect(CLUSTER_INDICATOR[state].type).toBe('in-progress')
    }
  })

  it('validates names and sizes as the API does', () => {
    expect(nameProblem('orders')).toBeNull()
    expect(nameProblem('')).not.toBeNull()
    expect(nameProblem('Orders')).not.toBeNull()
    expect(nameProblem('a--b')).not.toBeNull()
    expect(nameProblem('a'.repeat(41))).not.toBeNull()
    expect(nodeCountProblem(1)).toBeNull()
    expect(nodeCountProblem(10)).toBeNull()
    expect(nodeCountProblem(0)).not.toBeNull()
    expect(nodeCountProblem(11)).not.toBeNull()
    expect(nodeCountProblem(2.5)).not.toBeNull()
  })

  it('allows each action in the states the API allows it', () => {
    expect(canScale('active') && canScale('degraded')).toBe(true)
    expect(canScale('updating') || canScale('creating')).toBe(false)
    expect(canDownloadKubeconfig('creating')).toBe(false)
    expect(canDownloadKubeconfig('degraded')).toBe(true)
    expect(canDelete('failed') && canDelete('creating')).toBe(true)
    expect(canDelete('deleting')).toBe(false)
  })

  it('hides the Kubernetes section only when the module is off', () => {
    const sections = (kubernetes: boolean) =>
      navigationFor({ objectStorage: true, databases: true, kubernetes }).map((e) =>
        'text' in e ? e.text : '',
      )
    expect(sections(true)).toContain('Kubernetes')
    expect(sections(false)).not.toContain('Kubernetes')
  })
})

describe('ClustersListPage', () => {
  it('lists the clusters with their state, version and Ready nodes', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/kubernetes' })

    expect(await screen.findByRole('link', { name: 'orders' })).toBeInTheDocument()
    const orders = rowOf('orders')
    expect(within(orders).getByText('Active')).toBeInTheDocument()
    expect(within(orders).getByText('1.37')).toBeInTheDocument()
    expect(within(orders).getByText('3 / 3')).toBeInTheDocument()
    expect(within(orders).getByText('https://api.orders.default:6443')).toBeInTheDocument()
    expect(within(rowOf('billing')).getByText('Creating')).toBeInTheDocument()
    expect(within(rowOf('billing')).getByText('0 / 2')).toBeInTheDocument()
  })

  it('explains how to turn the module on when it is off', async () => {
    setMockKubernetes({ enabled: false, versions: [] })
    await signInAs()
    renderWithProviders(<Pages />, { route: '/kubernetes' })

    expect(await screen.findByText('Kubernetes is turned off on this node')).toBeInTheDocument()
    expect(screen.getByText('KUMO_KUBERNETES=on')).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
  })

  it('explains the image import when there is none', async () => {
    setMockKubernetes({ enabled: true, versions: [] })
    setMockClusters([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/kubernetes' })

    expect(await screen.findByText('No Kubernetes image is available yet')).toBeInTheDocument()
    expect(screen.getByRole('table')).toBeInTheDocument()
    expect(screen.getByTestId('create-cluster')).toBeDisabled()
  })

  it('keeps the write actions away from a viewer', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: '/kubernetes' })

    await screen.findByRole('link', { name: 'orders' })
    expect(screen.getByTestId('create-cluster')).toBeDisabled()
    expect(screen.getByTestId('delete-cluster')).toBeDisabled()
    expect(screen.queryByRole('radio')).not.toBeInTheDocument()
  })

  it('deletes a cluster only once its name is typed', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/kubernetes' })

    await screen.findByRole('link', { name: 'orders' })
    await userEvent.click(within(rowOf('orders')).getByRole('radio'))
    await userEvent.click(screen.getByTestId('delete-cluster'))

    expect(
      await screen.findByText('Everything on the cluster is deleted with it'),
    ).toBeInTheDocument()
    expect(screen.getByText('local-path')).toBeInTheDocument()
    const confirm = screen.getByTestId('confirm-delete-cluster')
    expect(confirm).toBeDisabled()
    await userEvent.type(screen.getByLabelText('Cluster name to confirm'), 'orders')
    await userEvent.click(confirm)

    await waitFor(() => {
      expect(mockClusters().find((c) => c.id === ORDERS)?.state).toBe('deleting')
    })
  })
})

describe('CreateClusterPage', () => {
  it('offers only the sizes kubeadm accepts', async () => {
    // Types either side of the limits: a control plane needs 2 vCPU, and every
    // node 2048 MiB.
    setMockInstanceTypes([
      { id: 'tiny', vcpus: 2, memory_mib: 1024, description: 'too little memory' },
      { id: 'single', vcpus: 1, memory_mib: 4096, description: 'one vCPU' },
      { id: 'kumo.medium', vcpus: 2, memory_mib: 4096, description: 'fits both' },
    ])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/kubernetes/create' })

    await screen.findByRole('heading', { name: 'Create cluster', level: 1 })
    const optionsOf = async (label: string) => {
      await userEvent.click(screen.getByRole('button', { name: new RegExp(label) }))
      const listed = screen.getAllByRole('option').map((o) => o.textContent ?? '')
      await userEvent.keyboard('{Escape}')
      return listed
    }
    const controlPlane = await optionsOf('Control plane size')
    expect(controlPlane.some((o) => o.startsWith('kumo.medium'))).toBe(true)
    expect(controlPlane.some((o) => o.startsWith('single'))).toBe(false)
    expect(controlPlane.some((o) => o.startsWith('tiny'))).toBe(false)
    const workers = await optionsOf('Worker size')
    expect(workers.some((o) => o.startsWith('single'))).toBe(true)
    expect(workers.some((o) => o.startsWith('tiny'))).toBe(false)
  })

  it('refuses a bad name before the round trip', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/kubernetes/create' })

    await userEvent.type(await screen.findByLabelText('Cluster name'), 'Orders')
    await userEvent.click(screen.getByTestId('submit-create-cluster'))
    expect(await screen.findByText(/Must start with a letter/)).toBeInTheDocument()
    expect(mockClusters()).toHaveLength(2)
  })

  it('creates a cluster and lands on its page', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/kubernetes/create' })

    await userEvent.type(await screen.findByLabelText('Cluster name'), 'shop')
    await userEvent.clear(screen.getByLabelText('Workers'))
    await userEvent.type(screen.getByLabelText('Workers'), '3')
    await userEvent.click(screen.getByTestId('submit-create-cluster'))

    expect(await screen.findByText('This cluster is being created')).toBeInTheDocument()
    const created = mockClusters().find((c) => c.name === 'shop')
    expect(created?.node_groups[0]?.desired_size).toBe(3)
    expect(created?.nodes).toHaveLength(4)
    await waitFor(() => {
      expect(store.getState().notifications.items[0]?.header).toBe('Cluster creation started')
    })
  })

  it('says why when there is no image', async () => {
    setMockKubernetes({ enabled: true, versions: [] })
    await signInAs()
    renderWithProviders(<Pages />, { route: '/kubernetes/create' })
    expect(await screen.findByText('No Kubernetes image is available yet')).toBeInTheDocument()
    expect(screen.queryByTestId('submit-create-cluster')).not.toBeInTheDocument()
  })
})

describe('ClusterDetailPage', () => {
  it('shows the cluster, its node group and its nodes', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: `/kubernetes/${ORDERS}` })

    expect(await screen.findByRole('heading', { name: 'orders', level: 1 })).toBeInTheDocument()
    expect(screen.getByText('100.64.0.0/16')).toBeInTheDocument()
    expect(screen.getByRole('table', { name: 'Node group' })).toBeInTheDocument()

    await userEvent.click(screen.getByRole('tab', { name: 'Nodes (3)' }))
    const nodes = screen.getByRole('table', { name: 'Nodes' })
    expect(within(nodes).getByText('orders-cp-1')).toBeInTheDocument()
    expect(within(nodes).getByText('Control plane')).toBeInTheDocument()
    expect(within(nodes).getAllByText('Ready')).toHaveLength(3)
  })

  it('downloads a kubeconfig in the group of the role', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: `/kubernetes/${ORDERS}` })

    await screen.findByRole('heading', { name: 'orders', level: 1 })
    await userEvent.click(screen.getByRole('tab', { name: 'Connect' }))
    expect(screen.getByText('kumo:admins')).toBeInTheDocument()
    expect(screen.getByText('Pods do not get NET_RAW')).toBeInTheDocument()
    await userEvent.click(screen.getByTestId('download-kubeconfig'))

    expect(await screen.findByText(/Downloaded; valid until/)).toBeInTheDocument()
    await waitFor(() => {
      expect(store.getState().notifications.items[0]?.header).toBe('Kubeconfig downloaded')
    })
  })

  it('gives a viewer a viewer kubeconfig, and no writes', async () => {
    await signInAs(MOCK_VIEWER)
    renderWithProviders(<Pages />, { route: `/kubernetes/${ORDERS}` })

    await screen.findByRole('heading', { name: 'orders', level: 1 })
    expect(screen.getByTestId('delete-cluster')).toBeDisabled()
    expect(screen.getByTestId('resize-node-group')).toBeDisabled()
    await userEvent.click(screen.getByRole('tab', { name: 'Connect' }))
    expect(screen.getByText('kumo:viewers')).toBeInTheDocument()
    expect(screen.getByTestId('download-kubeconfig')).toBeEnabled()
  })

  it('offers no kubeconfig before the cluster is up', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/kubernetes/eks-0b1c2d3e4f5a60718' })
    await screen.findByRole('heading', { name: 'billing', level: 1 })
    expect(screen.getByText('This cluster is being created')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('tab', { name: 'Connect' }))
    expect(screen.getByTestId('download-kubeconfig')).toBeDisabled()
  })

  it('resizes the node group, and warns about the drain when shrinking', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: `/kubernetes/${ORDERS}` })

    await screen.findByRole('heading', { name: 'orders', level: 1 })
    await userEvent.click(screen.getByTestId('resize-node-group'))
    const nodes = await screen.findByLabelText('Nodes')
    await userEvent.clear(nodes)
    await userEvent.type(nodes, '1')
    expect(screen.getByText('The newest workers are drained first')).toBeInTheDocument()
    await userEvent.click(screen.getByTestId('confirm-resize'))

    await waitFor(() => {
      const orders = mockClusters().find((c) => c.id === ORDERS)
      expect(orders?.state).toBe('updating')
      expect(orders?.node_groups[0]?.desired_size).toBe(1)
    })
    await waitFor(() => {
      expect(store.getState().notifications.items[0]?.header).toBe('Node group resizing')
    })
  })

  it('explains a degraded cluster with its reason', async () => {
    setMockClusters(
      mockClusters().map((c) =>
        c.id === ORDERS
          ? { ...c, state: 'degraded', state_reason: 'nodes not Ready: orders-default-2' }
          : c,
      ),
    )
    await signInAs()
    renderWithProviders(<Pages />, { route: `/kubernetes/${ORDERS}` })

    expect(await screen.findByText('This cluster is degraded')).toBeInTheDocument()
    expect(screen.getByText(/orders-default-2/)).toBeInTheDocument()
  })
})
