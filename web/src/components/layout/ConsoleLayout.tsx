import { useCallback, useMemo, useState } from 'react'
import { Outlet, useLocation, useNavigate } from 'react-router-dom'
import AppLayout from '@cloudscape-design/components/app-layout'
import TopNavigation from '@cloudscape-design/components/top-navigation'
import SideNavigation from '@cloudscape-design/components/side-navigation'
import BreadcrumbGroup from '@cloudscape-design/components/breadcrumb-group'
import Flashbar from '@cloudscape-design/components/flashbar'
import type { TopNavigationProps } from '@cloudscape-design/components'
import { useAppDispatch, useAppSelector } from '@/app/store'
import { usePageMetaContext } from '@/app/pageMeta'
import { toggleMode } from '@/app/theme'
import { useLogoutMutation, useSystemInfoQuery } from '@/features/auth/api'
import { selectUser } from '@/features/auth/authSlice'
import { dismiss } from '@/features/notifications/notificationsSlice'
import { useAutoDismiss } from '@/features/notifications/useAutoDismiss'
import { activeHref, navigationFor } from './navigation'

type Utilities = NonNullable<TopNavigationProps['utilities']>

/** SERVICE_HOME is where a Services menu entry leads. */
const SERVICE_HOME: Record<string, string> = {
  compute: '/instances',
  storage: '/buckets',
  network: '/vpcs',
  database: '/databases',
  kubernetes: '/kubernetes',
  spark: '/spark',
}

/**
 * servicesMenu lists the service groups. Storage is live on a node with object
 * storage on and marked "Not configured" otherwise; Databases is live unless
 * the module has been turned off.
 */
function servicesMenu(
  {
    objectStorage,
    databases,
    kubernetes,
    spark,
  }: { objectStorage: boolean; databases: boolean; kubernetes: boolean; spark: boolean },
  onOpen: (href: string) => void,
): Utilities {
  return [
    {
      type: 'menu-dropdown',
      text: 'Services',
      ariaLabel: 'Services',
      items: [
        { id: 'compute', text: 'Compute', description: 'Instances, images and key pairs' },
        objectStorage
          ? { id: 'storage', text: 'Storage', description: 'S3-compatible buckets' }
          : { id: 'storage', text: 'Storage', description: 'Not configured', disabled: true },
        databases
          ? { id: 'database', text: 'Databases', description: 'Managed Postgres' }
          : { id: 'database', text: 'Databases', description: 'Turned off', disabled: true },
        kubernetes
          ? { id: 'kubernetes', text: 'Kubernetes', description: 'Kubernetes clusters' }
          : { id: 'kubernetes', text: 'Kubernetes', description: 'Turned off', disabled: true },
        spark
          ? { id: 'spark', text: 'Spark', description: 'Spark clusters' }
          : { id: 'spark', text: 'Spark', description: 'Turned off', disabled: true },
        { id: 'network', text: 'Network', description: 'VPCs and subnets' },
      ],
      onItemClick: ({ detail }) => {
        const href = SERVICE_HOME[detail.id]
        if (href !== undefined) {
          onOpen(href)
        }
      },
    },
    { type: 'button', iconName: 'status-info', text: 'kumo-01', ariaLabel: 'This node' },
  ]
}

/** userMenu is the User menu: who is signed in, and the way out. */
function userMenu(username: string, onSignOut: () => void): Utilities {
  return [
    {
      type: 'menu-dropdown',
      text: username,
      iconName: 'user-profile',
      ariaLabel: 'User menu',
      items: [
        { id: 'profile', text: 'Profile' },
        { id: 'theme', text: 'Toggle dark mode' },
        { id: 'signout', text: 'Sign out' },
      ],
      onItemClick: ({ detail }) => {
        if (detail.id === 'theme') {
          toggleMode()
        }
        if (detail.id === 'signout') {
          onSignOut()
        }
      },
    },
  ]
}

/**
 * ConsoleLayout is the shell every console page renders in: the Cloudscape
 * AppLayout with the sticky top navigation, the side navigation, the page
 * breadcrumbs and the Flashbar.
 */
export function ConsoleLayout() {
  const location = useLocation()
  const navigate = useNavigate()
  const dispatch = useAppDispatch()
  const { breadcrumbs } = usePageMetaContext()
  const notifications = useAppSelector((state) => state.notifications.items)
  // Notifications retire on their own; see useAutoDismiss.
  useAutoDismiss(notifications)
  const username = useAppSelector(selectUser)?.username ?? 'User'
  const [logout] = useLogoutMutation()
  const { data: info } = useSystemInfoQuery()
  const objectStorage = info !== undefined && info.object_storage.backend !== 'none'
  // The databases module is on by default, so an unknown answer shows the
  // section rather than hiding it: a node that is still loading its info is
  // far more likely to have databases than not.
  const databases = info === undefined || info.databases.enabled
  // Kubernetes is on by default too, and reads the same way while loading.
  const kubernetes = info === undefined || info.kubernetes.enabled
  const spark = info === undefined || info.spark.enabled
  const navigation = useMemo(
    () => navigationFor({ objectStorage, databases, kubernetes, spark }),
    [objectStorage, databases, kubernetes, spark],
  )

  const [navigationOpen, setNavigationOpen] = useState(true)

  // Signing out is local first: the mutation clears the session and the query
  // cache even when the request fails, so no sign-out can leave the console
  // looking authenticated. A mutation trigger is called directly — `dispatch`
  // is for queries.
  const signOut = useCallback(() => {
    navigate('/login', { replace: true })
    void logout()
  }, [logout, navigate])

  // TopNavigation re-measures itself whenever `utilities` changes identity, so
  // the list has to be stable across renders.
  const utilities = useMemo(
    () => [
      ...servicesMenu({ objectStorage, databases, kubernetes, spark }, navigate),
      ...userMenu(username, signOut),
    ],
    [objectStorage, databases, kubernetes, spark, navigate, username, signOut],
  )

  return (
    <>
      <div id="h">
        <TopNavigation
          i18nStrings={{ overflowMenuTriggerText: 'All', overflowMenuTitleText: 'All' }}
          utilities={utilities}
          identity={{ href: '/', title: 'Kumo' }}
        />
      </div>

      <AppLayout
        headerSelector="#h"
        toolsHide
        navigationOpen={navigationOpen}
        onNavigationChange={({ detail }) => setNavigationOpen(detail.open)}
        navigation={
          <SideNavigation
            activeHref={activeHref(location.pathname)}
            header={{ href: '/', text: 'Kumo' }}
            items={navigation}
            onFollow={(event) => {
              if (event.detail.external || event.detail.href === undefined) {
                return
              }
              event.preventDefault()
              navigate(event.detail.href)
            }}
          />
        }
        breadcrumbs={
          <BreadcrumbGroup
            items={breadcrumbs ?? []}
            onFollow={(event) => {
              if (event.detail.href === undefined) {
                return
              }
              event.preventDefault()
              navigate(event.detail.href)
            }}
            ariaLabel="Breadcrumbs"
          />
        }
        notifications={
          <Flashbar
            items={notifications.map((item) => ({
              id: item.id,
              type: item.type,
              header: item.header,
              content: item.content,
              dismissible: item.dismissible,
              dismissLabel: 'Dismiss',
              statusIconAriaLabel: item.type,
              onDismiss: () => dispatch(dismiss(item.id)),
            }))}
            i18nStrings={{ ariaLabel: 'Notifications' }}
          />
        }
        content={<Outlet />}
        ariaLabels={{ navigation: 'Console navigation', notifications: 'Notifications' }}
      />
    </>
  )
}
