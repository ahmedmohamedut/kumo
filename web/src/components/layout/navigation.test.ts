import { describe, expect, it } from 'vitest'

import { activeHref, navigationItems } from './navigation'

describe('activeHref', () => {
  it.each([
    ['/', '/'],
    ['/instances', '/instances'],
    ['/instances/i-1', '/instances'],
    ['/images', '/images'],
    ['/key-pairs', '/key-pairs'],
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

    for (const route of ['/', '/instances', '/images', '/key-pairs']) {
      expect(hrefs).toContain(route)
    }
  })
})
