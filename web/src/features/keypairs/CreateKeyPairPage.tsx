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
import Select from '@cloudscape-design/components/select'
import SpaceBetween from '@cloudscape-design/components/space-between'
import TagEditor, { type TagEditorProps } from '@cloudscape-design/components/tag-editor'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppStore } from '@/app/store'
import { createKeyPair } from '@/features/keypairs/api'
import { PrivateKeyDialog } from '@/features/keypairs/PrivateKeyDialog'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'

type KeyPairType = 'ed25519' | 'rsa'

/**
 * generatedKey is the private key waiting to be saved. It lives in this
 * component's state for exactly as long as the dialog is open, and nowhere else:
 * not in Redux, not in the URL, not in sessionStorage.
 */
interface generatedKey {
  name: string
  privateKey: string
}

/**
 * CreateKeyPairPage generates a key pair and then shows its private key once.
 *
 * The order matters. `createKeyPair(...).unwrap()` hands the response to local
 * state and `reset()` immediately drops RTK Query's cached copy of it, so between
 * the two statements the store does not hold a private key even briefly. Only
 * after that does the save dialog appear, reading from state.
 */
export function CreateKeyPairPage() {
  const dispatch = useAppDispatch()
  const store = useAppStore()
  const navigate = useNavigate()

  const [isCreating, setCreating] = useState(false)
  const [error, setError] = useState<ApiError>(undefined)

  const [name, setName] = useState('')
  const [type, setType] = useState<KeyPairType>('ed25519')
  const [tags, setTags] = useState<ReadonlyArray<TagEditorProps.Tag>>([])
  const [generated, setGenerated] = useState<generatedKey | null>(null)

  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Key pairs', href: '/key-pairs' },
      { text: 'Create key pair', href: '/key-pairs/create' },
    ],
  })

  async function submit(event: FormEvent) {
    event.preventDefault()
    setCreating(true)
    setError(undefined)
    try {
      // `createKeyPair` goes out uncached, so this response — and the private key
      // in it — exists only as the value of `result` and the state set below.
      const result = await createKeyPair(
        {
          name,
          type,
          tags: Object.fromEntries(tags.map((tag) => [tag.key, tag.value])),
        },
        store,
      )

      if ('error' in result) {
        setError(result.error)
        return
      }
      setGenerated({ name: result.data.name, privateKey: result.data.private_key })
    } finally {
      setCreating(false)
    }
  }

  function onKeySaved() {
    dispatch(
      notify({
        type: 'success',
        header: 'Key pair created',
        content: `Key pair ${name} is ready. Save the private key you just downloaded — it cannot be shown again.`,
      }),
    )
    setGenerated(null)
    navigate('/key-pairs')
  }

  const errors = fieldErrors(error)

  return (
    <>
      <form onSubmit={submit}>
        <ContentLayout
          header={
            <Header
              variant="h1"
              description="Kumo generates the key pair on this node and shows the private key exactly once."
            >
              Create key pair
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
                  loading={isCreating}
                  data-testid="create-key-pair"
                >
                  Create key pair
                </Button>
              </SpaceBetween>
            }
          >
            <SpaceBetween size="l">
              {error !== undefined && (
                <Alert type="error" header="The key pair was not created">
                  {errorMessage(error, 'The key pair could not be created.')}
                </Alert>
              )}

              <Container header={<Header variant="h2">Key</Header>}>
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
                    label="Type"
                    description="ED25519 is the default and the faster of the two. RSA is for clients that insist on it."
                    errorText={errors.type}
                  >
                    <Select
                      selectedOption={{ value: type }}
                      onChange={({ detail }) => setType(detail.selectedOption.value as KeyPairType)}
                      options={[
                        { value: 'ed25519', label: 'ED25519' },
                        { value: 'rsa', label: 'RSA (4096 bit)' },
                      ]}
                      ariaLabel="Type"
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

      {generated !== null && (
        <PrivateKeyDialog
          visible
          name={generated.name}
          privateKey={generated.privateKey}
          onDismiss={onKeySaved}
        />
      )}
    </>
  )
}
