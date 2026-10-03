import { Navigate, createBrowserRouter } from 'react-router-dom'

import { ConsoleLayout } from '@/components/layout/ConsoleLayout'
import { DashboardPage } from '@/features/dashboard/DashboardPage'
import { ImagesListPage } from '@/features/images/ImagesListPage'
import { InstancesListPage } from '@/features/instances/InstancesListPage'
import { KeyPairsListPage } from '@/features/keypairs/KeyPairsListPage'
import { NotFoundPage } from '@/features/system/NotFoundPage'

/**
 * Console routes from 03-frontend.md §3. `/login` and `/setup` arrive with the
 * auth milestone; until then the console is open, which only affects a
 * development build.
 */
export const router = createBrowserRouter([
  {
    path: '/',
    element: <ConsoleLayout />,
    children: [
      { index: true, element: <DashboardPage /> },
      { path: 'instances', element: <InstancesListPage /> },
      { path: 'images', element: <ImagesListPage /> },
      { path: 'key-pairs', element: <KeyPairsListPage /> },
      { path: '*', element: <NotFoundPage /> },
    ],
  },
  {
    path: '*',
    element: <Navigate replace to="/" />,
  },
])
