import { PlaceholderPage } from '@/components/layout/PlaceholderPage'
import { usePageMeta } from '@/app/pageMeta'

/** KeyPairsList lists SSH key pairs (M2.2). */
export function KeyPairsListPage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Key pairs', href: '/key-pairs' },
    ],
  })

  return (
    <PlaceholderPage
      title="Key pairs"
      description="SSH key pairs instances can be launched with"
      milestone="M2.2"
    />
  )
}
