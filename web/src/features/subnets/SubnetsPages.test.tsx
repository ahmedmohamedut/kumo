import { describe, expect, it } from 'vitest'
import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { Route, Routes } from 'react-router-dom'

import { CreateSubnetPage } from '@/features/subnets/CreateSubnetPage'
import { SubnetDetailPage } from '@/features/subnets/SubnetDetailPage'
import { SubnetsListPage } from '@/features/subnets/SubnetsListPage'
import { mockSubnets } from '@/mocks/handlers'
import { renderWithProviders, signInAs } from '@/test/utils'

function Pages() {
  return (
    <Routes>
      <Route path="/subnets" element={<SubnetsListPage />} />
      <Route path="/subnets/create" element={<CreateSubnetPage />} />
      <Route path="/subnets/:subnetId" element={<SubnetDetailPage />} />
      <Route path="/vpcs/:vpcId" element={<p>VPC detail</p>} />
    </Routes>
  )
}

/** rowOfCell finds the table row holding a cell with the given text. The subnet
 *  name and its connected VPC can both spell "default", so rows are anchored on
 *  the unique IPv4 CIDR cell instead of a name link. */
function rowOfCell(text: string): HTMLElement {
  const cell = screen.getByText(text)
  const row = cell.closest('tr')
  if (row === null) {
    throw new Error(`no table row holding "${text}"`)
  }
  return row
}

describe('SubnetsListPage', () => {
  it('lists the subnets with their VPC, CIDR and free addresses', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/subnets' })

    expect(await screen.findByRole('link', { name: 'app' })).toBeInTheDocument()

    const app = rowOfCell('172.16.0.0/24')
    // The VPC name is resolved from the same lookup the filter is built from.
    const vpcLink = within(app).getByRole('link', { name: 'prod' })
    expect(vpcLink).toHaveAttribute('href', '/vpcs/vpc-0b2c3d4e5f6071890')
    // 253 usable, nothing allocated yet.
    expect(within(app).getByText('253')).toBeInTheDocument()

    // The default subnet carries the badge.
    const def = rowOfCell('10.0.0.0/24')
    expect(within(def).getByText('Default')).toBeInTheDocument()
    expect(within(rowOfCell('10.0.1.0/24')).queryByText('Default')).not.toBeInTheDocument()
  })

  it('filters the table down to one VPC and keeps the choice in the URL', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/subnets' })

    await screen.findByRole('link', { name: 'app' })
    await userEvent.click(screen.getByRole('button', { name: /Filter by VPC/ }))
    await userEvent.click(await screen.findByRole('option', { name: /prod/ }))

    await waitFor(() => {
      expect(screen.queryByRole('link', { name: 'default' })).not.toBeInTheDocument()
    })
    expect(screen.getByRole('link', { name: 'app' })).toBeInTheDocument()
  })

  it('sends the operator to the create form', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/subnets' })

    await screen.findByRole('link', { name: 'app' })
    await userEvent.click(screen.getByTestId('create-subnet'))

    expect(await screen.findByRole('heading', { name: 'Create subnet' })).toBeInTheDocument()
  })

  it('asks before deleting and removes the empty subnet once confirmed', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/subnets' })

    // `app` has nothing allocated from it.
    await screen.findByRole('link', { name: 'app' })
    await userEvent.click(within(rowOfCell('172.16.0.0/24')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-subnets'))

    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('heading', { name: 'Delete app?' })).toBeInTheDocument()
    expect(mockSubnets().map((subnet) => subnet.name)).toContain('app')

    await userEvent.click(screen.getByTestId('confirm-delete-subnets'))

    await waitFor(() => expect(screen.queryByRole('link', { name: 'app' })).not.toBeInTheDocument())
    expect(mockSubnets().map((subnet) => subnet.name)).not.toContain('app')
    expect(store.getState().notifications.items[0]?.type).toBe('success')
  })

  it('names the allocated addresses that block deleting a subnet when the API answers 409', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/subnets' })

    // The default subnet has two addresses handed out (web-1, web-2).
    await screen.findByRole('link', { name: 'app' })
    await userEvent.click(within(rowOfCell('10.0.0.0/24')).getByRole('checkbox'))
    await userEvent.click(screen.getByTestId('delete-subnets'))
    await userEvent.click(screen.getByTestId('confirm-delete-subnets'))

    expect(await screen.findByRole('link', { name: 'app' })).toBeInTheDocument()
    const notifications = store.getState().notifications.items
    expect(notifications).toHaveLength(1)
    expect(notifications[0]?.type).toBe('error')
    expect(notifications[0]?.content).toContain('2 allocated addresses')
  })
})

describe('CreateSubnetPage', () => {
  it('creates the subnet and follows it to its detail page', async () => {
    await signInAs()
    const { store } = renderWithProviders(<Pages />, { route: '/subnets/create' })

    // Choose the default VPC, then give the subnet a block inside it.
    await userEvent.click(screen.getByRole('button', { name: /Choose a VPC/ }))
    await userEvent.click(await screen.findByRole('option', { name: /default/ }))
    await userEvent.type(screen.getByRole('textbox', { name: 'Name' }), 'redis')
    await userEvent.type(screen.getByRole('textbox', { name: 'IPv4 CIDR' }), '10.0.2.0/24')
    await userEvent.click(screen.getByTestId('submit-create-subnet'))

    expect(await screen.findByRole('heading', { level: 1, name: 'redis' })).toBeInTheDocument()
    const subnet = mockSubnets().find((candidate) => candidate.name === 'redis')
    expect(subnet?.vpc_id).toBe('vpc-0a1b2c3d4e5f60718')
    expect(subnet?.cidr).toBe('10.0.2.0/24')
    expect(subnet?.total_ips).toBe(256)
    expect(subnet?.usable_ips).toBe(253)
    expect(store.getState().notifications.items[0]?.type).toBe('success')
  })

  it('lands a CIDR outside the VPC on the cidr field', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/subnets/create' })

    await userEvent.click(screen.getByRole('button', { name: /Choose a VPC/ }))
    await userEvent.click(await screen.findByRole('option', { name: /default/ }))
    await userEvent.type(screen.getByRole('textbox', { name: 'Name' }), 'stray')
    // 172.16.0.0/16 lives in the `prod` VPC, not the default one.
    await userEvent.type(screen.getByRole('textbox', { name: 'IPv4 CIDR' }), '172.16.9.0/24')
    await userEvent.click(screen.getByTestId('submit-create-subnet'))

    expect(
      await screen.findByText(/must sit entirely inside the VPC "default"/),
    ).toBeInTheDocument()
    expect(mockSubnets().map((subnet) => subnet.name)).not.toContain('stray')
  })

  it('refuses a CIDR that overlaps a sibling and lands the conflict on the page', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/subnets/create' })

    await userEvent.click(screen.getByRole('button', { name: /Choose a VPC/ }))
    await userEvent.click(await screen.findByRole('option', { name: /default/ }))
    await userEvent.type(screen.getByRole('textbox', { name: 'Name' }), 'overlap')
    // 10.0.0.0/24 is the default subnet next door.
    await userEvent.type(screen.getByRole('textbox', { name: 'IPv4 CIDR' }), '10.0.0.0/24')
    await userEvent.click(screen.getByTestId('submit-create-subnet'))

    expect(await screen.findByText(/the CIDR overlaps the subnet "default"/)).toBeInTheDocument()
    expect(mockSubnets().map((subnet) => subnet.name)).not.toContain('overlap')
  })
})

describe('SubnetDetailPage', () => {
  it('shows the address counts, the parent VPC and what the interface holds', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/subnets/subnet-0a1b2c3d4e5f60718' })

    expect(await screen.findByRole('heading', { level: 1, name: 'default' })).toBeInTheDocument()

    // The parent VPC round-trips to its own page.
    const vpcLink = screen.getByRole('link', { name: 'vpc-0a1b2c3d4e5f60718' })
    expect(vpcLink).toHaveAttribute('href', '/vpcs/vpc-0a1b2c3d4e5f60718')

    expect(screen.getByText('10.0.0.0/24')).toBeInTheDocument()
    expect(screen.getByText('256')).toBeInTheDocument() // total
    expect(screen.getByText('253')).toBeInTheDocument() // usable
    expect(screen.getByText('2')).toBeInTheDocument() // allocated (web-1, web-2)
    expect(screen.getByText('251')).toBeInTheDocument() // available
    expect(screen.getByText('Default')).toBeInTheDocument()
  })

  it('refuses to delete while an address is allocated, and says so on the page', async () => {
    await signInAs()
    renderWithProviders(<Pages />, { route: '/subnets/subnet-0a1b2c3d4e5f60718' })

    await screen.findByRole('heading', { level: 1, name: 'default' })
    await userEvent.click(screen.getByTestId('delete-subnet'))
    await userEvent.click(screen.getByTestId('confirm-delete-subnet'))

    expect(await screen.findByText(/still has 2 allocated addresses/)).toBeInTheDocument()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(mockSubnets().map((subnet) => subnet.name)).toContain('default')
  })
})
