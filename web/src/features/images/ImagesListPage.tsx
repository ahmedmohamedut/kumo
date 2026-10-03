import { PlaceholderPage } from '@/components/layout/PlaceholderPage'
import { usePageMeta } from '@/app/pageMeta'

/** ImagesList lists imported images with their import progress (M3.3). */
export function ImagesListPage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Images', href: '/images' },
    ],
  })

  return (
    <PlaceholderPage
      title="Images"
      description="Guest images available to launch instances from"
      milestone="M3.3"
    />
  )
}
