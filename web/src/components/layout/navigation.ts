import type { SideNavigationProps } from '@cloudscape-design/components'

type NavItem = NonNullable<SideNavigationProps['items']>[number]

/**
 * The Storage section (06-object-storage.md). It is shown only on a node with
 * object storage on; see navigationFor.
 */
const storageSection: NavItem = {
  type: 'section',
  text: 'Storage',
  defaultExpanded: true,
  items: [
    { type: 'link', text: 'Buckets', href: '/buckets' },
    { type: 'link', text: 'Access keys', href: '/access-keys' },
  ],
}

/**
 * The Databases section (07-databases.md). It is shown only on a node with the
 * module on; see navigationFor. A node with the module on and no engine image
 * keeps it: the pages explain the one import an operator has to do, which is
 * the opposite of a section that could only fail.
 */
const databasesSection: NavItem = {
  type: 'section',
  text: 'Databases',
  defaultExpanded: true,
  items: [{ type: 'link', text: 'Databases', href: '/databases' }],
}

/**
 * Side navigation from 03-frontend.md §2: every section a node can have.
 * activeHref matches against all of it, so a Storage page is still the active
 * entry while the system info that reveals the section is loading.
 */
export const navigationItems: ReadonlyArray<NavItem> = [
  { type: 'link', text: 'Dashboard', href: '/' },
  {
    type: 'section',
    text: 'Compute',
    defaultExpanded: true,
    items: [
      { type: 'link', text: 'Instances', href: '/instances' },
      { type: 'link', text: 'Instance types', href: '/instance-types' },
      { type: 'link', text: 'Images', href: '/images' },
      { type: 'link', text: 'Key pairs', href: '/key-pairs' },
    ],
  },
  {
    type: 'section',
    text: 'Networking',
    defaultExpanded: true,
    items: [
      { type: 'link', text: 'VPCs', href: '/vpcs' },
      { type: 'link', text: 'Subnets', href: '/subnets' },
    ],
  },
  storageSection,
  databasesSection,
]

/**
 * navigationFor is the navigation a node shows: Storage only when object
 * storage is configured and Databases only when the databases module is on,
 * because those pages would otherwise only explain that they are off.
 */
export function navigationFor({
  objectStorage,
  databases,
}: {
  objectStorage: boolean
  databases: boolean
}): NavItem[] {
  return navigationItems.filter(
    (entry) =>
      (objectStorage || entry !== storageSection) && (databases || entry !== databasesSection),
  )
}

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
