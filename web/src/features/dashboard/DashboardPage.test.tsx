import { describe, expect, it } from 'vitest'
import { screen, within } from '@testing-library/react'
import { Route, Routes } from 'react-router-dom'

import { DashboardPage } from '@/features/dashboard/DashboardPage'
import { mockInstances, setMockCapacity, setMockInstances, setMockTasks } from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

function Pages() {
  return (
    <Routes>
      <Route path="/" element={<DashboardPage />} />
      <Route path="/instances" element={<p>Instances list</p>} />
      <Route path="/instances/launch" element={<p>Launch instance</p>} />
    </Routes>
  )
}

/** tally reads one of the instance counts by its test id. */
function tally(id: string): HTMLElement {
  return screen.getByTestId(`tally-${id}`)
}

describe('DashboardPage', () => {
  it('counts the instances by state', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/' })

    // The fixtures hold one running, one pending, one error and one terminated.
    expect(await screen.findByTestId('tally-total')).toHaveTextContent('3')
    expect(tally('running')).toHaveTextContent('1')
    expect(tally('in-progress')).toHaveTextContent('1')
    expect(tally('error')).toHaveTextContent('1')
    expect(tally('stopped')).toHaveTextContent('0')
  })

  it('leaves a terminated instance out of the counts', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/' })

    // A terminate deletes the row now, so `terminated` is a leftover of an
    // earlier release and describes nothing the node is running. Counting it
    // would also put the total above the sum of the tiles beside it.
    expect(await screen.findByTestId('tally-total')).toHaveTextContent('3')
    expect(mockInstances().some((instance) => instance.state === 'terminated')).toBe(true)
  })

  it('shows a zero rather than hiding a state nothing is in', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/' })

    // "Nothing is in error" is worth reading as a number. A tile that vanished
    // when its count hit zero would also reflow the row under an operator
    // watching a boot.
    expect(await screen.findByTestId('tally-stopped')).toHaveTextContent('0')
    expect(screen.getByText('Stopped')).toBeInTheDocument()
  })

  it('offers a launch on a node with no instances', async () => {
    setMockInstances([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/' })

    expect(await screen.findByText(/No instances yet/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Launch an instance' })).toBeInTheDocument()
  })

  it('reports capacity as what is allocated against what the node has', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/' })

    // 3 of 32 vCPUs, 3 GiB of 16, 30 GiB of 200 in the fixtures.
    expect(await screen.findByText('3 of 32 allocated')).toBeInTheDocument()
    expect(screen.getByText('3.0 GiB of 16.0 GiB allocated')).toBeInTheDocument()
    expect(screen.getByText('30.0 GiB of 200.0 GiB allocated')).toBeInTheDocument()

    // Free is derived, never stored (00-architecture.md §6).
    expect(screen.getByText('29 left')).toBeInTheDocument()
  })

  it('says so when a metric is over-allocated', async () => {
    // Allowed by design: vCPUs are offered at an overcommit ratio, so a
    // memory- or disk-bound node can hand out more than it has. The endpoint
    // reports it rather than hiding it, and so must the page.
    setMockCapacity({
      vcpus: { total: 8, allocated: 12 },
      memory_bytes: { total: 1073741824, allocated: 2147483648 },
      disk_bytes: { total: 1073741824, allocated: 536870912 },
    })
    await signInAs()
    renderWithProviders(<Pages />, { route: '/' })

    expect(await screen.findByText('Over-allocated by 4')).toBeInTheDocument()
    expect(screen.getByText('Over-allocated by 1.0 GiB')).toBeInTheDocument()

    // The bar cannot draw past full, so the figure has to carry the overflow.
    // The one metric that is not over-allocated still reads normally.
    expect(screen.getByText('512.0 MiB left')).toBeInTheDocument()
  })

  it('lists the most recent tasks with how they went', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/' })

    // Scoped to the panel: "Running" is also an instance tally label, so an
    // unscoped query would pass on the wrong element.
    const panel = within(await screen.findByTestId('recent-tasks'))

    // The fixtures carry two succeeded tasks, one running and one failed, and
    // the API serves them newest first.
    expect(panel.getByText('Failed')).toBeInTheDocument()
    expect(panel.getAllByText('Succeeded')).toHaveLength(2)
    expect(panel.getByText('Running')).toBeInTheDocument()
    expect(panel.getAllByText('image.import')).toHaveLength(4)
  })

  it('shows a failed task error instead of its age', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/' })

    // An operator reading a failed task wants the reason, not when it started.
    expect(
      await screen.findByText('the source returned 403 for the requested image'),
    ).toBeInTheDocument()
  })

  it('says nothing has run on a node with no tasks', async () => {
    setMockTasks([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/' })

    expect(await screen.findByText(/Nothing has run on this node yet/)).toBeInTheDocument()
  })

  it('links to the instances table', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/' })

    const link = await screen.findByRole('link', { name: 'View all instances' })
    expect(link).toHaveAttribute('href', '/instances')
  })
})
