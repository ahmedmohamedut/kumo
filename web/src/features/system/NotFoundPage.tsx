import Box from '@cloudscape-design/components/box'
import ContentLayout from '@cloudscape-design/components/content-layout'
import Header from '@cloudscape-design/components/header'
import Button from '@cloudscape-design/components/button'
import SpaceBetween from '@cloudscape-design/components/space-between'
import { useNavigate } from 'react-router-dom'
import { usePageMeta } from '@/app/pageMeta'

/** NotFound is rendered for every route the console does not know. */
export function NotFoundPage() {
  const navigate = useNavigate()
  usePageMeta({ breadcrumbs: [{ text: 'Not found', href: '/not-found' }] })

  return (
    <ContentLayout
      header={
        <Header variant="h1" description="This page does not exist in the Kumo console.">
          404
        </Header>
      }
    >
      <SpaceBetween size="m">
        <Button variant="primary" onClick={() => navigate('/')}>
          Back to dashboard
        </Button>
        <Box padding="m">The console has no page at this address.</Box>
      </SpaceBetween>
    </ContentLayout>
  )
}
