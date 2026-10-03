import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from 'react'
import type { BreadcrumbGroupProps } from '@cloudscape-design/components'

export type Breadcrumbs = BreadcrumbGroupProps['items']

export interface PageMeta {
  /** Breadcrumbs shown above the page content, most recent last. */
  breadcrumbs?: Breadcrumbs
}

interface PageMetaContextValue {
  meta: PageMeta
  setMeta: (meta: PageMeta) => void
}

const defaultMeta: PageMeta = { breadcrumbs: [] }

const PageMetaContext = createContext<PageMetaContextValue>({
  meta: defaultMeta,
  setMeta: () => undefined,
})

/**
 * PageMetaProvider holds the breadcrumbs of the page rendered inside
 * `ConsoleLayout`. Pages declare theirs with `usePageMeta` instead of threading
 * them through props.
 */
export function PageMetaProvider({ children }: { children: ReactNode }) {
  const [meta, setMeta] = useState<PageMeta>(defaultMeta)
  const value = useMemo(() => ({ meta, setMeta }), [meta])

  return <PageMetaContext.Provider value={value}>{children}</PageMetaContext.Provider>
}

/**
 * usePageMeta declares the breadcrumbs of the calling page.
 *
 * Pages pass an inline array, so the effect keys off the values rather than the
 * array identity — otherwise every render would publish a new object and the
 * layout would re-render forever.
 */
export function usePageMeta({ breadcrumbs = [] }: PageMeta): void {
  const { setMeta } = useContext(PageMetaContext)
  const key = breadcrumbs.map((item) => `${item.text}>${item.href}`).join('|')

  useEffect(() => {
    setMeta({ breadcrumbs })
    // eslint-disable-next-line react-hooks/exhaustive-deps -- keyed by values above
  }, [key, setMeta])
}

/** usePageMetaContext is what `ConsoleLayout` reads. */
export function usePageMetaContext(): PageMeta {
  return useContext(PageMetaContext).meta
}
