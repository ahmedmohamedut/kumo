import { useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Button from '@cloudscape-design/components/button'
import Container from '@cloudscape-design/components/container'
import ContentLayout from '@cloudscape-design/components/content-layout'
import Form from '@cloudscape-design/components/form'
import FormField from '@cloudscape-design/components/form-field'
import Header from '@cloudscape-design/components/header'
import Input from '@cloudscape-design/components/input'
import SpaceBetween from '@cloudscape-design/components/space-between'
import TagEditor, { type TagEditorProps } from '@cloudscape-design/components/tag-editor'
import Textarea from '@cloudscape-design/components/textarea'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch } from '@/app/store'
import { useImportKeyPairMutation } from '@/features/keypairs/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors } from '@/lib/apiError'

/**
 * ImportKeyPairPage registers a public key the operator already has.
 *
 * Nothing here ever sees a private key: the field takes an `authorized_keys`
 * line, and the server rejects private key material outright rather than
 * trusting this page to have filtered it (keys.go).
 */
export function ImportKeyPairPage() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const [importKeyPair, { isLoading, error }] = useImportKeyPairMutation()

  const [name, setName] = useState('')
  const [publicKey, setPublicKey] = useState('')
  const [tags, setTags] = useState<ReadonlyArray<TagEditorProps.Tag>>([])

  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Key pairs', href: '/key-pairs' },
      { text: 'Import key pair', href: '/key-pairs/import' },
    ],
  })

  async function submit(event: FormEvent) {
    event.preventDefault()
    try {
      const imported = await importKeyPair({
        name,
        public_key: publicKey,
        tags: Object.fromEntries(tags.map((tag) => [tag.key, tag.value])),
      }).unwrap()
      dispatch(
        notify({
          type: 'success',
          header: 'Key pair imported',
          content: `Key pair ${imported.name} is ready to launch instances with.`,
        }),
      )
      navigate('/key-pairs')
    } catch {
      // The Alert and the field errors below report it.
    }
  }

  const errors = fieldErrors(error)

  return (
    <form onSubmit={submit}>
      <ContentLayout
        header={
          <Header
            variant="h1"
            description="Register a public key you already hold. Only the public half is stored."
          >
            Import key pair
          </Header>
        }
      >
        <Form
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button variant="link" onClick={() => navigate('/key-pairs')} formAction="submit">
                Cancel
              </Button>
              <Button
                variant="primary"
                formAction="submit"
                loading={isLoading}
                data-testid="import-key-pair"
              >
                Import key pair
              </Button>
            </SpaceBetween>
          }
        >
          <SpaceBetween size="l">
            {error !== undefined && (
              <Alert type="error" header="The key pair was not imported">
                {errorMessage(error, 'The key pair could not be imported.')}
              </Alert>
            )}

            <Container header={<Header variant="h2">Public key</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="Key pair name"
                  description="Letters, digits, '.', '_' and '-'. The name must be unique on this node."
                  errorText={errors.name}
                  constraintText="1-64 characters"
                >
                  <Input
                    value={name}
                    onChange={({ detail }) => setName(detail.value)}
                    ariaLabel="Key pair name"
                    autoFocus
                  />
                </FormField>

                <FormField
                  label="Public key"
                  description="One authorized_keys line, e.g. the output of `cat ~/.ssh/id_ed25519.pub`. A private key is rejected."
                  errorText={errors.public_key}
                  constraintText="ssh-ed25519 or ssh-rsa, at least 2048 bits for RSA"
                >
                  <Textarea
                    value={publicKey}
                    onChange={({ detail }) => setPublicKey(detail.value)}
                    rows={4}
                    ariaLabel="Public key"
                  />
                </FormField>

                <FormField
                  label="Tags"
                  description="Optional key/value pairs to organize key pairs."
                  errorText={errors.tags}
                >
                  <TagEditor
                    tags={tags}
                    onChange={({ detail }) => setTags(detail.tags)}
                    i18nStrings={{ keyPlaceholder: 'Enter a tag key' }}
                    tagLimit={50}
                  />
                </FormField>
              </SpaceBetween>
            </Container>
          </SpaceBetween>
        </Form>
      </ContentLayout>
    </form>
  )
}
