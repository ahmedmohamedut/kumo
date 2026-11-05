import { describe, expect, it } from 'vitest'

import { activeHref, navigationFor, navigationItems } from './navigation'

describe('activeHref', () => {
  it.each([
    ['/', '/'],
    ['/instances', '/instances'],
    ['/instances/i-1', '/instances'],
    ['/instance-types', '/instance-types'],
    ['/instance-types/kumo.small/edit', '/instance-types'],
    ['/images', '/images'],
    ['/key-pairs', '/key-pairs'],
    ['/vpcs', '/vpcs'],
    ['/vpcs/vpc-1', '/vpcs'],
    ['/vpcs/create', '/vpcs'],
    ['/subnets', '/subnets'],
    ['/subnets/subnet-1', '/subnets'],
    ['/buckets', '/buckets'],
    ['/buckets/app-logs', '/buckets'],
    ['/buckets/create', '/buckets'],
    ['/access-keys', '/access-keys'],
    ['/access-keys/KUMO1/edit', '/access-keys'],
    ['/databases', '/databases'],
    ['/databases/db-1', '/databases'],
    ['/databases/create', '/databases'],
    ['/unknown', undefined],
    ['/instancesx', undefined],
  ])('maps %s to %s', (pathname, want) => {
    expect(activeHref(pathname)).toBe(want)
  })

  it('offers an entry for every navigation route', () => {
    const hrefs: string[] = []
    for (const entry of navigationItems) {
      if (entry.type === 'link') {
        hrefs.push(entry.href)
        continue
      }
      const children = 'items' in entry ? entry.items : []
      for (const child of children) {
        if (child.type === 'link') {
          hrefs.push(child.href)
        }
      }
    }

    for (const route of [
      '/',
      '/instances',
      '/instance-types',
      '/images',
      '/key-pairs',
      '/vpcs',
      '/subnets',
      '/buckets',
      '/access-keys',
      '/databases',
    ]) {
      expect(hrefs).toContain(route)
    }
  })
})

describe('navigationFor', () => {
  const sections = (objectStorage: boolean, databases = true) =>
    navigationFor({ objectStorage, databases }).flatMap((entry) =>
      entry.type === 'section' ? [entry.text] : [],
    )

  it('adds Storage when object storage is on', () => {
    expect(sections(true)).toEqual(['Compute', 'Networking', 'Storage', 'Databases'])
  })

  it('leaves Storage out when it is off', () => {
    expect(sections(false)).toEqual(['Compute', 'Networking', 'Databases'])
  })

  it('leaves Databases out when the module is off', () => {
    expect(sections(true, false)).toEqual(['Compute', 'Networking', 'Storage'])
  })

  // A node with neither module is the smallest console there is.
  it('keeps Compute and Networking with both modules off', () => {
    expect(sections(false, false)).toEqual(['Compute', 'Networking'])
  })
})
