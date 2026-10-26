import { useState, type FormEvent } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Button from '@cloudscape-design/components/button'
import Container from '@cloudscape-design/components/container'
import ContentLayout from '@cloudscape-design/components/content-layout'
import Form from '@cloudscape-design/components/form'
import FormField from '@cloudscape-design/components/form-field'
import Header from '@cloudscape-design/components/header'
import Input from '@cloudscape-design/components/input'
import Multiselect from '@cloudscape-design/components/multiselect'
import RadioGroup from '@cloudscape-design/components/radio-group'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch, useAppSelector, useAppStore } from '@/app/store'
import { selectIsAdmin } from '@/features/auth/authSlice'
import {
  createAccessKey,
  useGetAccessKeyQuery,
  useUpdateAccessKeyMutation,
  type AccessKey,
  type AccessLevel,
  type CreatedAccessKey,
} from '@/features/accesskeys/api'
import { SecretKeyDialog } from '@/features/accesskeys/SecretKeyDialog'
import { useGetBucketQuery, useListBucketsQuery } from '@/features/buckets/api'
import { ObjectStorageGate } from '@/features/buckets/ObjectStorageGate'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'

const NAME_RE = /^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$/

/** CreateAccessKeyPage issues a key, then shows its secret once. */
export function CreateAccessKeyPage() {
  usePageMeta({
    breadcrumbs: [
      { text: 'Storage', href: '/buckets' },
      { text: 'Access keys', href: '/access-keys' },
      { text: 'Create access key', href: '/access-keys/create' },
    ],
  })
  return (
    <ObjectStorageGate title="Create access key">
      <AccessKeyForm />
    </ObjectStorageGate>
  )
}

/** EditAccessKeyPage renames or re-scopes a key; its secret is unchanged. */
export function EditAccessKeyPage() {
  const { accessKeyId } = useParams<{ accessKeyId: string }>()
  const id = accessKeyId ?? ''
  const { data, error, isLoading } = useGetAccessKeyQuery(id, { skip: id === '' })

  usePageMeta({
    breadcrumbs: [
      { text: 'Storage', href: '/buckets' },
      { text: 'Access keys', href: '/access-keys' },
      { text: data?.name ?? id, href: `/access-keys/${encodeURIComponent(id)}/edit` },
    ],
  })

  return (
    <ObjectStorageGate title="Edit access key">
      {isLoading ? (
        <Spinner size="large" />
      ) : data === undefined ? (
        <Alert type="error" header="The access key could not be loaded">
          {errorMessage(error, 'The access key could not be loaded.')}
        </Alert>
      ) : (
        <AccessKeyForm existing={data} />
      )}
    </ObjectStorageGate>
  )
}

/**
 * AccessKeyForm is the create and edit form: a name, the access level and
 * the buckets. The bucket picker offers what exists, which is also what the
 * API accepts.
 */
function AccessKeyForm({ existing }: { existing?: AccessKey }) {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()
  const isAdmin = useAppSelector(selectIsAdmin)
  const { data: buckets, isLoading: bucketsLoading } = useListBucketsQuery()

  const [name, setName] = useState(existing?.name ?? '')
  const [level, setLevel] = useState<AccessLevel>(existing?.level ?? 'read-only')
  const [picked, setPicked] = useState<string[]>(existing?.buckets ?? [])
  const [error, setError] = useState<ApiError>(undefined)
  const [localErrors, setLocalErrors] = useState<Record<string, string>>({})
  const [created, setCreated] = useState<CreatedAccessKey | null>(null)

  const store = useAppStore()
  const [isCreating, setCreating] = useState(false)
  const [updateAccessKey, { isLoading: isUpdating }] = useUpdateAccessKeyMutation()
  // The endpoint and region of the first bucket complete the snippet in the
  // secret dialog; every bucket reports the same ones.
  const { data: firstBucket } = useGetBucketQuery(created?.buckets[0] ?? '', {
    skip: created === null,
  })

  const errors = { ...fieldErrors(error), ...localErrors }

  async function submit(event: FormEvent) {
    event.preventDefault()
    setError(undefined)
    const invalid: Record<string, string> = {}
    if (!NAME_RE.test(name.trim())) {
      invalid['name'] =
        'Must be 1-64 letters, digits, dots, dashes or underscores, starting with a letter or digit.'
    }
    if (picked.length === 0) {
      invalid['buckets'] = 'Choose at least one bucket.'
    }
    setLocalErrors(invalid)
    if (Object.keys(invalid).length > 0) {
      return
    }

    try {
      if (existing === undefined) {
        setCreating(true)
        try {
          const result = await createAccessKey({ name: name.trim(), level, buckets: picked }, store)
          if ('error' in result) {
            setError(result.error)
            return
          }
          setCreated(result.data)
        } finally {
          setCreating(false)
        }
        return
      }
      await updateAccessKey({
        id: existing.access_key,
        name: name.trim(),
        level,
        buckets: picked,
      }).unwrap()
      dispatch(notify({ type: 'success', header: 'Access key updated', content: name.trim() }))
      navigate('/access-keys')
    } catch (err) {
      setError(err as ApiError)
    }
  }

  const options = (buckets ?? []).map((bucket) => ({ value: bucket.name, label: bucket.name }))
  // A granted bucket that has since been deleted is still shown, so saving
  // the form does not quietly drop it.
  for (const bucket of picked) {
    if (!options.some((option) => option.value === bucket)) {
      options.push({ value: bucket, label: bucket })
    }
  }

  // The dialog is a sibling of the form, not a child: a modal is portalled in
  // the DOM but not in React, and a click inside it would submit the form.
  return (
    <>
      <form onSubmit={submit}>
        <ContentLayout
          header={
            <Header
              variant="h1"
              description={
                existing === undefined
                  ? 'An access key lets an application use the chosen buckets with any S3 client. Its secret is shown once.'
                  : 'A change of access or buckets applies to the key’s next request. The secret stays the same.'
              }
            >
              {existing === undefined ? 'Create access key' : `Edit ${existing.name}`}
            </Header>
          }
        >
          <Form
            actions={
              <SpaceBetween direction="horizontal" size="s">
                <Button variant="link" formAction="none" onClick={() => navigate('/access-keys')}>
                  Cancel
                </Button>
                <Button
                  variant="primary"
                  formAction="submit"
                  disabled={!isAdmin}
                  loading={isCreating || isUpdating}
                  data-testid="submit-access-key"
                >
                  {existing === undefined ? 'Create access key' : 'Save changes'}
                </Button>
              </SpaceBetween>
            }
          >
            <SpaceBetween size="l">
              {!isAdmin && (
                <Alert type="info">Only an administrator can issue or change access keys.</Alert>
              )}
              {error !== undefined && Object.keys(fieldErrors(error)).length === 0 && (
                <Alert type="error" header="The access key was not saved">
                  {errorMessage(error, 'The access key could not be saved.')}
                </Alert>
              )}

              <Container header={<Header variant="h2">Access key</Header>}>
                <SpaceBetween size="m">
                  <FormField
                    label="Name"
                    description="Which application holds the key, so you know what breaks if you delete it."
                    errorText={errors['name']}
                  >
                    <Input
                      value={name}
                      onChange={({ detail }) => setName(detail.value)}
                      ariaLabel="Name"
                      placeholder="web-app"
                    />
                  </FormField>

                  <FormField label="Access" errorText={errors['level']}>
                    <RadioGroup
                      value={level}
                      onChange={({ detail }) => setLevel(detail.value as AccessLevel)}
                      items={[
                        {
                          value: 'read-only',
                          label: 'Read only',
                          description: 'List the buckets and read objects.',
                        },
                        {
                          value: 'read-write',
                          label: 'Read and write',
                          description:
                            'Also upload and delete objects. Never create, delete or reconfigure a bucket.',
                        },
                      ]}
                    />
                  </FormField>

                  <FormField
                    label="Buckets"
                    description="The key reaches these buckets and nothing else."
                    errorText={errors['buckets']}
                  >
                    <Multiselect
                      selectedOptions={options.filter((option) => picked.includes(option.value))}
                      onChange={({ detail }) =>
                        setPicked(
                          detail.selectedOptions.flatMap((option) =>
                            option.value === undefined ? [] : [option.value],
                          ),
                        )
                      }
                      options={options}
                      statusType={bucketsLoading ? 'loading' : 'finished'}
                      loadingText="Loading buckets"
                      placeholder="Choose buckets"
                      filteringType="auto"
                      ariaLabel="Buckets"
                      empty="No buckets yet. Create one first."
                    />
                  </FormField>
                </SpaceBetween>
              </Container>
            </SpaceBetween>
          </Form>
        </ContentLayout>
      </form>
      {created !== null && (
        <SecretKeyDialog
          created={created}
          {...(firstBucket === undefined
            ? {}
            : { endpoint: firstBucket.endpoint, region: firstBucket.region })}
          onDismiss={() => {
            setCreated(null)
            dispatch(
              notify({ type: 'success', header: 'Access key created', content: name.trim() }),
            )
            navigate('/access-keys')
          }}
        />
      )}
    </>
  )
}
