import type { SideNavigationProps } from '@cloudscape-design/components'

type NavItem = NonNullable<SideNavigationProps['items']>[number]

/**
 * Side navigation from 03-frontend.md §2. Compute is the only service group in
 * this phase; the others show up as their milestones land.
 */
export const navigationItems: ReadonlyArray<NavItem> = [
  { type: 'link', text: 'Dashboard', href: '/' },
  {
    type: 'section',
    text: 'Compute',
    defaultExpanded: true,
    items: [
      { type: 'link', text: 'Instances', href: '/instances' },
      { type: 'link', text: 'Images', href: '/images' },
      { type: 'link', text: 'Key pairs', href: '/key-pairs' },
    ],
  },
]

/** activeHref is the navigation entry matching the current route, if any. */
export function activeHref(pathname: string): string | undefined {
  for (const entry of navigationItems) {
    if (entry.type === 'section' || entry.type === 'section-group') {
      for (const child of entry.items ?? []) {
        if (child.type === 'link' && isActive(child.href, pathname)) {
          return child.href
        }
      }
      continue
    }
    if (entry.type === 'link' && isActive(entry.href, pathname)) {
      return entry.href
    }
  }
  return undefined
}

/** isActive reports whether href is the current route. */
function isActive(href: string, pathname: string): boolean {
  return href === '/' ? pathname === '/' : pathname === href || pathname.startsWith(`${href}/`)
}
