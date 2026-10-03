import Box from '@cloudscape-design/components/box'
import ContentLayout from '@cloudscape-design/components/content-layout'
import Header from '@cloudscape-design/components/header'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Alert from '@cloudscape-design/components/alert'
import type { ReactNode } from 'react'

export interface PlaceholderPageProps {
  title: string
  /** Which roadmap milestone fills this page in. */
  milestone: string
  description: string
  /** Actions rendered above the placeholder, once the page has some. */
  actions?: ReactNode
}

/**
 * PlaceholderPage backs every console route that exists for navigation before
 * its feature milestone. It keeps the real Cloudscape layout (header,
 * description, spacing) so the pages only grow content later.
 */
export function PlaceholderPage({ title, milestone, description, actions }: PlaceholderPageProps) {
  return (
    <ContentLayout
      header={
        <Header variant="h1" description={description}>
          {title}
        </Header>
      }
    >
      <SpaceBetween size="m">
        {actions}
        <Alert type="info" statusIconAriaLabel="Info">
          {title} arrives with {milestone}. The route, its navigation entry and the API client base
          are already wired.
        </Alert>
        <Box padding="m">No content yet.</Box>
      </SpaceBetween>
    </ContentLayout>
  )
}
