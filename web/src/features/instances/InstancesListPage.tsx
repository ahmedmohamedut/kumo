import { PlaceholderPage } from '@/components/layout/PlaceholderPage'
import { usePageMeta } from '@/app/pageMeta'

/** InstancesList is the instance table with the state actions (M4.6). */
export function InstancesListPage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Instances', href: '/instances' },
    ],
  })

  return (
    <PlaceholderPage
      title="Instances"
      description="Virtual machines on this node"
      milestone="M4.6"
    />
  )
}
