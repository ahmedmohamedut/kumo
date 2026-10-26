import { useState } from 'react'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import Checkbox from '@cloudscape-design/components/checkbox'
import ColumnLayout from '@cloudscape-design/components/column-layout'
import CopyToClipboard from '@cloudscape-design/components/copy-to-clipboard'
import FormField from '@cloudscape-design/components/form-field'
import Modal from '@cloudscape-design/components/modal'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Textarea from '@cloudscape-design/components/textarea'

import type { CreatedAccessKey } from '@/features/accesskeys/api'

export interface SecretKeyDialogProps {
  /** The new key, with its secret, held by the caller in state and nowhere else. */
  created: CreatedAccessKey
  /** endpoint and region, when known, complete the environment snippet. */
  endpoint?: string
  region?: string
  onDismiss: () => void
}

/**
 * SecretKeyDialog is the only time a secret access key is shown. Like the
 * private key dialog, it cannot be closed until the operator says they saved
 * it: Kumo keeps no copy, and a lost secret means issuing a new key.
 */
export function SecretKeyDialog({ created, endpoint, region, onDismiss }: SecretKeyDialogProps) {
  const [saved, setSaved] = useState(false)

  const env = [
    `AWS_ACCESS_KEY_ID=${created.access_key}`,
    `AWS_SECRET_ACCESS_KEY=${created.secret_key}`,
    ...(endpoint === undefined ? [] : [`AWS_ENDPOINT_URL=${endpoint}`]),
    ...(region === undefined ? [] : [`AWS_REGION=${region}`]),
  ].join('\n')

  return (
    <Modal
      visible
      onDismiss={() => {
        if (saved) {
          onDismiss()
        }
      }}
      header="Save your secret access key"
      closeAriaLabel={saved ? 'Close' : 'Save your secret access key before closing'}
      size="large"
      footer={
        <Box float="right">
          <Button variant="primary" disabled={!saved} onClick={onDismiss} data-testid="secret-done">
            I have saved it
          </Button>
        </Box>
      }
    >
      <SpaceBetween size="m">
        <Alert type="warning" header="The secret access key is shown only once">
          Kumo does not keep a copy. If you lose it, delete this key and issue a new one.
        </Alert>

        <ColumnLayout columns={2} variant="text-grid">
          <SpaceBetween size="xxs">
            <Box variant="awsui-key-label">Access key</Box>
            <CopyToClipboard
              variant="inline"
              textToCopy={created.access_key}
              copyButtonAriaLabel="Copy access key"
              copySuccessText="Access key copied"
              copyErrorText="Access key not copied"
            />
          </SpaceBetween>
          <SpaceBetween size="xxs">
            <Box variant="awsui-key-label">Secret access key</Box>
            <CopyToClipboard
              variant="inline"
              textToCopy={created.secret_key}
              copyButtonAriaLabel="Copy secret access key"
              copySuccessText="Secret access key copied"
              copyErrorText="Secret access key not copied"
            />
          </SpaceBetween>
        </ColumnLayout>

        <FormField
          label="As environment variables"
          description="For the AWS CLI and SDKs. Keep wherever you put them readable only by the application."
          secondaryControl={
            <CopyToClipboard
              variant="icon"
              textToCopy={env}
              copyButtonAriaLabel="Copy environment variables"
              copySuccessText="Environment variables copied"
              copyErrorText="Environment variables not copied"
            />
          }
        >
          <Textarea value={env} readOnly rows={4} ariaLabel="Environment variables" />
        </FormField>

        <Checkbox
          checked={saved}
          onChange={({ detail }) => setSaved(detail.checked)}
          data-testid="secret-saved"
        >
          I have saved the secret access key somewhere safe
        </Checkbox>
      </SpaceBetween>
    </Modal>
  )
}
