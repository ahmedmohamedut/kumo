import { PlaceholderPage } from '@/components/layout/PlaceholderPage'
import { usePageMeta } from '@/app/pageMeta'

/** Dashboard shows instance counts, capacity and recent tasks (M5). */
export function DashboardPage() {
  usePageMeta({ breadcrumbs: [{ text: 'Dashboard', href: '/' }] })

  return (
    <PlaceholderPage
      title="Dashboard"
      description="Instances by state, capacity and recent tasks"
      milestone="M5"
    />
  )
}
