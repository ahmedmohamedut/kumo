import { useState } from 'react'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import Checkbox from '@cloudscape-design/components/checkbox'
import FormField from '@cloudscape-design/components/form-field'
import Modal from '@cloudscape-design/components/modal'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Textarea from '@cloudscape-design/components/textarea'

import { downloadPrivateKey, pemFileName } from '@/features/keypairs/download'

export interface PrivateKeyDialogProps {
  visible: boolean
  /** The key pair name, used for the download filename. */
  name: string
  /** The PEM, held by the caller in component state and nowhere else. */
  privateKey: string
  onDismiss: () => void
}

/**
 * PrivateKeyDialog is the one and only presentation of a generated private key.
 *
 * Closing it destroys the key: there is no way to ask for it again, and none is
 * kept on the server. So the close button stays disabled until the operator
 * confirms they saved it, which is the difference between "I read it" and "I have
 * the file". Copying to the clipboard is offered alongside the download because a
 * terminal is often where the key is headed next, but the download is what makes
 * the checkbox honest.
 */
export function PrivateKeyDialog({ visible, name, privateKey, onDismiss }: PrivateKeyDialogProps) {
  const [saved, setSaved] = useState(false)
  const [copied, setCopied] = useState(false)

  async function copy() {
    try {
      await navigator.clipboard.writeText(privateKey)
      setCopied(true)
    } catch {
      // A clipboard the browser refuses (no permission, insecure context) must
      // not look like a successful copy, so the confirmation stays off and the
      // operator is left with the download button.
      setCopied(false)
    }
  }

  return (
    <Modal
      visible={visible}
      // Cloudscape has no "non-dismissable" switch, so the guard lives here: the
      // overlay, Escape and the close icon all route through onDismiss, and until
      // the operator confirms the save they must not destroy the only copy.
      onDismiss={() => {
        if (saved) {
          onDismiss()
        }
      }}
      header="Save your private key"
      closeAriaLabel={saved ? 'Close' : 'Save your private key before closing'}
      footer={
        <Box float="right">
          <Button
            variant="primary"
            disabled={!saved}
            onClick={onDismiss}
            data-testid="private-key-done"
          >
            I have saved it
          </Button>
        </Box>
      }
    >
      <SpaceBetween size="m">
        <Alert type="warning" header="This private key is shown only once">
          Kumo does not keep a copy. If you close this dialog without saving the key, the only way
          to log in with it again is to create a new key pair.
        </Alert>

        <FormField
          label="Private key"
          description="OpenSSH private key in PEM form. Keep the file readable only by you (0600)."
        >
          <Textarea
            value={privateKey}
            readOnly
            rows={8}
            ariaLabel="Private key"
            data-testid="private-key-value"
          />
        </FormField>

        <SpaceBetween direction="horizontal" size="s">
          <Button
            onClick={() => downloadPrivateKey(name, privateKey)}
            iconName="download"
            data-testid="private-key-download"
          >
            Download {pemFileName(name)}
          </Button>
          <Button onClick={copy} iconName="copy" data-testid="private-key-copy">
            {copied ? 'Copied' : 'Copy'}
          </Button>
        </SpaceBetween>

        {copied && (
          <Box variant="small" color="text-status-success">
            Copied to the clipboard. The download is still the safer copy.
          </Box>
        )}

        <Checkbox
          checked={saved}
          onChange={({ detail }) => setSaved(detail.checked)}
          data-testid="private-key-saved"
        >
          I have saved this key somewhere safe
        </Checkbox>
      </SpaceBetween>
    </Modal>
  )
}
