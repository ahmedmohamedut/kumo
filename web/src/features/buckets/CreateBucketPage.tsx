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
import Toggle from '@cloudscape-design/components/toggle'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector } from '@/app/store'
import { selectIsAdmin } from '@/features/auth/authSlice'
import { useCreateBucketMutation } from '@/features/buckets/api'
import { ObjectStorageGate } from '@/features/buckets/ObjectStorageGate'
import { bucketNameProblem } from '@/features/buckets/state'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'

/** CreateBucketPage is the Create bucket form, behind the object storage gate. */
export function CreateBucketPage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Storage', href: '/buckets' },
      { text: 'Buckets', href: '/buckets' },
      { text: 'Create bucket', href: '/buckets/create' },
    ],
  })

  return (
    <ObjectStorageGate title="Create bucket">
      <CreateBucketForm />
    </ObjectStorageGate>
  )
}

/**
 * CreateBucketForm creates a bucket. The API answers 201 with the bucket
 * ready, versioning applied, so the page goes straight to its detail page.
 */
function CreateBucketForm() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)

  const [name, setName] = useState('')
  const [versioning, setVersioning] = useState(false)
  const [tags, setTags] = useState<ReadonlyArray<TagEditorProps.Tag>>([])
  const [error, setError] = useState<ApiError>(undefined)
  const [localErrors, setLocalErrors] = useState<Record<string, string>>({})

  const [createBucket, { isLoading }] = useCreateBucketMutation()

  const errors = { ...fieldErrors(error), ...localErrors }

  async function submit(event: FormEvent) {
    event.preventDefault()
    setError(undefined)

    const problem = bucketNameProblem(name.trim())
    const invalid: Record<string, string> = problem === null ? {} : { name: problem }
    setLocalErrors(invalid)
    if (Object.keys(invalid).length > 0) {
      return
    }

    try {
      const bucket = await createBucket({
        name: name.trim(),
        versioning,
        ...(tags.length > 0
          ? { tags: Object.fromEntries(tags.map((tag) => [tag.key, tag.value ?? ''])) }
          : {}),
      }).unwrap()
      dispatch(notify({ type: 'success', header: 'Bucket created', content: bucket.name }))
      navigate(`/buckets/${encodeURIComponent(bucket.name)}`)
    } catch (err) {
      setError(err as ApiError)
    }
  }

  return (
    <form onSubmit={submit}>
      <ContentLayout
        header={
          <Header
            variant="h1"
            description="A bucket holds objects that applications read and write with any S3 client."
          >
            Create bucket
          </Header>
        }
      >
        <Form
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button variant="link" onClick={() => navigate('/buckets')} formAction="none">
                Cancel
              </Button>
              <Button
                variant="primary"
                formAction="submit"
                loading={isLoading}
                loadingText="Creating"
                disabled={!isAdmin}
                data-testid="submit-create-bucket"
              >
                Create bucket
              </Button>
            </SpaceBetween>
          }
        >
          <SpaceBetween size="l">
            {!isAdmin && <Alert type="info">Only an administrator can create buckets.</Alert>}
            {error !== undefined && Object.keys(errors).length === 0 && (
              <Alert type="error" header="The bucket was not created">
                {errorMessage(error, 'The bucket could not be created.')}
              </Alert>
            )}

            <Container header={<Header variant="h2">General configuration</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="Bucket name"
                  description="Unique on this object storage server, and part of every object's URL. It cannot be changed later."
                  constraintText="3-63 characters: lowercase letters, digits, dots and dashes."
                  errorText={errors['name']}
                >
                  <Input
                    value={name}
                    onChange={({ detail }) => setName(detail.value)}
                    ariaLabel="Bucket name"
                    placeholder="app-logs"
                    autoFocus
                  />
                </FormField>

                <FormField
                  label="Versioning"
                  description="Keep every version of an object, so an overwrite or a delete can be undone. Old versions take space until they are deleted."
                >
                  <Toggle
                    checked={versioning}
                    onChange={({ detail }) => setVersioning(detail.checked)}
                  >
                    Enable versioning
                  </Toggle>
                </FormField>
              </SpaceBetween>
            </Container>

            <Container header={<Header variant="h2">Tags</Header>}>
              <FormField
                label="Tags"
                description="Optional key/value pairs to organize buckets."
                errorText={errors['tags']}
              >
                <TagEditor
                  tags={tags}
                  onChange={({ detail }) => setTags(detail.tags)}
                  i18nStrings={{ keyPlaceholder: 'Enter a tag key' }}
                  tagLimit={50}
                />
              </FormField>
            </Container>
          </SpaceBetween>
        </Form>
      </ContentLayout>
    </form>
  )
}
