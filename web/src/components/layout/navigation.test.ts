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
    ]) {
      expect(hrefs).toContain(route)
    }
  })
})

describe('navigationFor', () => {
  const sections = (objectStorage: boolean) =>
    navigationFor({ objectStorage }).flatMap((entry) =>
      entry.type === 'section' ? [entry.text] : [],
    )

  it('adds Storage when object storage is on', () => {
    expect(sections(true)).toEqual(['Compute', 'Networking', 'Storage'])
  })

  it('leaves Storage out when it is off', () => {
    expect(sections(false)).toEqual(['Compute', 'Networking'])
  })
})
