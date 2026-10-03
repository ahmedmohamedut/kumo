import { Navigate, createBrowserRouter } from 'react-router-dom'

import { ConsoleLayout } from '@/components/layout/ConsoleLayout'
import { RequireAuth } from '@/features/auth/RequireAuth'
import { LoginPage } from '@/features/auth/LoginPage'
import { SetupPage } from '@/features/auth/SetupPage'
import { DashboardPage } from '@/features/dashboard/DashboardPage'
import { ImagesListPage } from '@/features/images/ImagesListPage'
import { InstancesListPage } from '@/features/instances/InstancesListPage'
import { KeyPairsListPage } from '@/features/keypairs/KeyPairsListPage'
import { NotFoundPage } from '@/features/system/NotFoundPage'

/**
 * Console routes from 03-frontend.md §3. `/login` and `/setup` are public, and
 * every console route sits behind `RequireAuth`, which redirects to
 * `/login?next=…` while the session is anonymous.
 */
export const router = createBrowserRouter([
  { path: '/login', element: <LoginPage /> },
  { path: '/setup', element: <SetupPage /> },
  {
    path: '/',
    element: (
      <RequireAuth>
        <ConsoleLayout />
      </RequireAuth>
    ),
    children: [
      { index: true, element: <DashboardPage /> },
      { path: 'instances', element: <InstancesListPage /> },
      { path: 'images', element: <ImagesListPage /> },
      { path: 'key-pairs', element: <KeyPairsListPage /> },
      { path: '*', element: <NotFoundPage /> },
    ],
  },
  {
    // A stray deep link should land somewhere useful, not on the dashboard.
    path: '*',
    element: <Navigate replace to="/" />,
  },
])
