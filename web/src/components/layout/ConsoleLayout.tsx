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
import { useLogoutMutation } from '@/features/auth/api'
import { selectUser } from '@/features/auth/authSlice'
import { dismiss } from '@/features/notifications/notificationsSlice'
import { useAutoDismiss } from '@/features/notifications/useAutoDismiss'
import { activeHref, navigationItems } from './navigation'

type Utilities = NonNullable<TopNavigationProps['utilities']>

const SERVICES_MENU: Utilities = [
  {
    type: 'menu-dropdown',
    text: 'Services',
    ariaLabel: 'Services',
    items: [
      { id: 'compute', text: 'Compute', description: 'Instances, images and key pairs' },
      { id: 'storage', text: 'Storage', description: 'Coming soon', disabled: true },
      { id: 'network', text: 'Network', description: 'Coming soon', disabled: true },
    ],
  },
  { type: 'button', iconName: 'status-info', text: 'kumo-01', ariaLabel: 'This node' },
]

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
    () => [...SERVICES_MENU, ...userMenu(username, signOut)],
    [username, signOut],
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
            items={navigationItems}
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
