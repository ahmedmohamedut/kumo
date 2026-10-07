import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { CreateVpcPage } from '@/features/vpcs/CreateVpcPage'
import { VpcDetailPage } from '@/features/vpcs/VpcDetailPage'
import { VpcsListPage } from '@/features/vpcs/VpcsListPage'
import { mockSubnets, mockVPCs, setMockSubnets } from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

function Pages() {
  return (
    <Routes>
      <Route path="/vpcs" element={<VpcsListPage />} />
      <Route path="/vpcs/create" element={<CreateVpcPage />} />
      <Route path="/vpcs/:vpcId" element={<VpcDetailPage />} />
      <Route path="/subnets/:subnetId" element={<p>Subnet detail</p>} />
      <Route path="/subnets/create" element={<p>Create subnet</p>} />
    </Routes>
  )
}

/** rowOf finds the table row holding a VPC name. */
function rowOf(name: string): HTMLElement {
  const cell = screen.getByRole('link', { name })
  const row = cell.closest('tr')
  if (row === null) {
    throw new Error(`no table row for ${name}`)
  }
  return row
}

describe('VpcsListPage', () => {
  it('lists the VPCs newest first with their address block and connectivity', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/vpcs' })

    expect(await screen.findByRole('link', { name: 'prod' })).toBeInTheDocument()

    const prod = rowOf('prod')
    expect(within(prod).getByText('172.16.0.0/16')).toBeInTheDocument()
    expect(within(prod).getByText('172.16.0.1')).toBeInTheDocument()
    expect(within(prod).getByText('Internet (NAT)')).toBeInTheDocument()

    // Only the default VPC carries the badge; a marker for every row would make
    // the column noise.
    const def = rowOf('default')
    expect(within(def).getByText('Default')).toBeInTheDocument()
    expect(within(prod).queryByText('Default')).not.toBeInTheDocument()

    // Newest first, the API's default order.
    const names = screen
      .getAllByRole('link', { name: /^(default|prod)$/ })
      .map((link) => link.textContent)
    expect(names).toEqual(['prod', 'default'])
  })

  it('sends the operator to the create form', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/vpcs' })

    await screen.findByRole('link', { name: 'default' })
    await userEvent.click(screen.getByTestId('create-vpc'))

    expect(await screen.findByRole('heading', { name: 'Create VPC' })).toBeInTheDocument()
  })

  it('asks before deleting, and only queues the delete once confirmed', async () => {
    // `prod` owns the `app` subnet, which would make its delete refuse with a
    // 409; strip the subnets so the test exercises the happy path.
    setMockSubnets([])
    await signInAs()
    renderWithProviders(<Pages />, { route: '/vpcs' })

    // `prod` has no subnets or instances (once the subnets are stripped), so it
    // is deletable.
    await screen.findByRole('link', { name: 'prod' })
    expect(screen.getByTestId('delete-vpcs')).toBeDisabled()

    await userEvent.click(within(rowOf('prod')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-vpcs'))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('heading', { name: 'Delete prod?' })).toBeInTheDocument()
    // The confirmation is not a side effect: the VPC is still there.
    expect(mockVPCs().map((vpc) => vpc.name)).toContain('prod')

    await userEvent.click(screen.getByTestId('confirm-delete-vpcs'))

    // A delete is a task, answered 202; the fixture row goes away at once.
    await waitFor(() =>
      expect(screen.queryByRole('link', { name: 'prod' })).not.toBeInTheDocument(),
    )
    expect(mockVPCs().map((vpc) => vpc.name)).not.toContain('prod')
  })

  it('names the subnet that blocks deleting its VPC when the API answers 409', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/vpcs' })

    // The default VPC has `default` and `db` subnets inside it.
    await screen.findByRole('link', { name: 'default' })
    await userEvent.click(within(rowOf('default')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-vpcs'))
    await userEvent.click(screen.getByTestId('confirm-delete-vpcs'))

    expect(await screen.findByRole('link', { name: 'default' })).toBeInTheDocument()
    const notifications = store.getState().notifications.items
    expect(notifications).toHaveLength(1)
    expect(notifications[0]?.type).toBe('error')
    expect(notifications[0]?.content).toContain('default')
    expect(notifications[0]?.content).toContain('"db"')
  })

  it('names the instance that blocks deleting its VPC when the API answers 409', async () => {
    // Strip the subnets so the default VPC's delete gets past the first guard
    // and lands on the instance one.
    setMockSubnets([])
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/vpcs' })

    await screen.findByRole('link', { name: 'default' })
    await userEvent.click(within(rowOf('default')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-vpcs'))
    await userEvent.click(screen.getByTestId('confirm-delete-vpcs'))

    expect(await screen.findByRole('link', { name: 'default' })).toBeInTheDocument()
    const notifications = store.getState().notifications.items
    expect(notifications[0]?.type).toBe('error')
    expect(notifications[0]?.content).toContain('web-1')
  })
})

describe('CreateVpcPage', () => {
  it('creates the VPC and follows the new VPC to its detail page', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/vpcs/create' })

    await userEvent.type(screen.getByRole('textbox', { name: 'Name' }), 'staging')
    await userEvent.type(screen.getByRole('textbox', { name: 'IPv4 CIDR' }), '192.168.10.0/24')
    await userEvent.click(screen.getByTestId('submit-create-vpc'))

    // The 202 carries the id of the task that brings the network up; the page
    // follows the row, not the task.
    const created = await screen.findByRole('heading', { level: 1, name: 'staging' })
    expect(created).toBeInTheDocument()
    const vpc = mockVPCs().find((candidate) => candidate.name === 'staging')
    expect(vpc?.cidr).toBe('192.168.10.0/24')
    expect(vpc?.gateway_ip).toBe('192.168.10.1')
    expect(vpc?.netmask).toBe('255.255.255.0')
    expect(store.getState().notifications.items[0]?.type).toBe('in-progress')
  })

  it('refuses a CIDR the node already owns and lands the complaint on the page', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/vpcs/create' })

    // 10.0.128.0/16 sits inside the default VPC's 10.0.0.0/16.
    await userEvent.type(screen.getByRole('textbox', { name: 'Name' }), 'overlap')
    await userEvent.type(screen.getByRole('textbox', { name: 'IPv4 CIDR' }), '10.0.128.0/16')
    await userEvent.click(screen.getByTestId('submit-create-vpc'))

    expect(await screen.findByText(/the CIDR overlaps the VPC "default"/)).toBeInTheDocument()
    expect(mockVPCs().map((vpc) => vpc.name)).not.toContain('overlap')
  })

  it('refuses to create with a red name field instead of a round trip', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/vpcs/create' })

    await userEvent.type(screen.getByRole('textbox', { name: 'Name' }), 'Bad_Name')
    await userEvent.type(screen.getByRole('textbox', { name: 'IPv4 CIDR' }), '192.168.10.0/24')
    await userEvent.click(screen.getByTestId('submit-create-vpc'))

    expect(
      await screen.findByText(/must be 1-63 characters of lowercase letters/i),
    ).toBeInTheDocument()
    expect(mockVPCs()).toHaveLength(2)
  })
})

/** nextValue reads the value that follows a details-grid label, e.g.
 *  "Subnets" in the row "Subnets / 2". The Row component renders the label and
 *  its value as the two children of one SpaceBetween stack. The container
 *  header below the grid also spells "Subnets", so the heading is skipped. */
function nextValue(label: string): string | null {
  const labelElement = screen.getAllByText(label).find((element) => element.tagName !== 'H2')
  if (labelElement === undefined) {
    throw new Error(`no details-grid label "${label}"`)
  }
  return labelElement.parentElement?.nextElementSibling?.textContent ?? null
}

describe('VpcDetailPage', () => {
  it('shows the address block, the counters and the subnets inside', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/vpcs/vpc-0a1b2c3d4e5f60718' })

    expect(await screen.findByRole('heading', { level: 1, name: 'default' })).toBeInTheDocument()

    expect(screen.getByText('10.0.0.0/16')).toBeInTheDocument()
    expect(screen.getByText('10.0.0.1')).toBeInTheDocument()
    expect(screen.getByText('255.255.0.0')).toBeInTheDocument()
    expect(nextValue('Subnets')).toContain('2')
    expect(nextValue('Instances')).toContain('3')
    expect(screen.getByText('kmb0a1b2c3d')).toBeInTheDocument()

    // The subnets round-trip: from the VPC page to its subnets and back.
    const subnetLink = screen.getByRole('link', { name: 'db' })
    expect(subnetLink).toHaveAttribute('href', '/subnets/subnet-0b2c3d4e5f6071890')

    // The subnets are the addresses carved out, with what is left of each.
    expect(screen.getByText('10.0.1.0/24')).toBeInTheDocument()
    expect(screen.getByText('252 IPs available')).toBeInTheDocument()
  })

  it('refuses to delete while the VPC is in use, and names the subnets on the page', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/vpcs/vpc-0a1b2c3d4e5f60718' })

    await screen.findByRole('heading', { level: 1, name: 'default' })
    await userEvent.click(screen.getByTestId('delete-vpc'))
    await userEvent.click(screen.getByTestId('confirm-delete-vpc'))

    // The 409 stays on the page instead of navigating away with the error.
    expect(
      await screen.findByText(/still contains the subnets "default", "db"/),
    ).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(mockVPCs().map((vpc) => vpc.name)).toContain('default')

    // And the remaining subnets are still there to prove it.
    expect(mockSubnets().map((subnet) => subnet.name)).toEqual(
      expect.arrayContaining(['default', 'db']),
    )
  })
})
