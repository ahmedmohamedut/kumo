import type { ReactNode } from 'react'
import Box from '@cloudscape-design/components/box'
import Container from '@cloudscape-design/components/container'

/**
 * AuthPage is the frame the login and setup pages share: a Container centered
 * on the plain Cloudscape background, which is what 03-frontend.md §4 asks for.
 */
export function AuthPage({
  title,
  description,
  children,
}: {
  title: string
  description: string
  children: ReactNode
}) {
  return (
    <Box textAlign="center" padding={{ top: 'xxl', bottom: 'xxl' }}>
      <div style={{ maxWidth: 480, margin: '0 auto', textAlign: 'left' }}>
        <Container header={<h1>{title}</h1>} variant="stacked">
          <p>{description}</p>
          {children}
        </Container>
      </div>
    </Box>
  )
}
