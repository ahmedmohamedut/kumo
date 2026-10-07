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
import { useAppDispatch } from '@/app/store'
import { useCreateVPCMutation, type VPCForwardMode } from '@/features/vpcs/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'

/** CIDR_RE accepts any dotted-quad/prefix pair; whether it is private and
 *  non-overlapping is the API's call. */
const CIDR_RE = /^(\d{1,3}\.){3}\d{1,3}\/\d{1,2}$/

/**
 * CreateVpcPage creates a VPC: a private address block plus the libvirt network
 * that carries it.
 *
 * The submit does not wait for the network: the API answers `202` with the row
 * already in place and the id of the task that defines and starts the network,
 * and the page goes straight to the detail page so the operator watches it come
 * up. A form sitting on a spinner until `virsh net-create` finishes would look
 * broken and would hold the browser hostage to a libvirt call that is not the
 * browser's business.
 */
export function CreateVpcPage() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()

  const [name, setName] = useState('')
  const [cidr, setCidr] = useState('')
  const [mode, setMode] = useState<VPCForwardMode>('nat')
  const [tags, setTags] = useState<ReadonlyArray<TagEditorProps.Tag>>([])
  const [error, setError] = useState<ApiError>(undefined)
  const [localErrors, setLocalErrors] = useState<Record<string, string>>({})

  const [createVPC, { isLoading: isCreating }] = useCreateVPCMutation()

  usePageMeta({
    breadcrumbs: [
      { text: 'Networking', href: '/' },
      { text: 'VPCs', href: '/vpcs' },
      { text: 'Create VPC', href: '/vpcs/create' },
    ],
  })

  // Local validation wins over the API's: a form that is fixed and resubmitted
  // must not keep showing the complaint of the previous attempt.
  const errors = { ...fieldErrors(error), ...localErrors }

  function validate(): Record<string, string> {
    const invalid: Record<string, string> = {}

    if (name.trim() === '') {
      invalid['name'] = 'Enter a name for the VPC.'
    } else if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name.trim()) || name.trim().length > 63) {
      // The API's rule: an RFC 1123 label that seeds the DNS domain. Stated here
      // so the red field arrives before the round trip rather than after it.
      invalid['name'] =
        'Must be 1-63 characters of lowercase letters, digits and dashes, starting and ending with a letter or digit.'
    }

    if (cidr.trim() === '') {
      invalid['cidr'] = 'Enter an IPv4 CIDR block for the VPC.'
    } else if (!CIDR_RE.test(cidr.trim())) {
      invalid['cidr'] = 'Enter a CIDR block like 10.0.0.0/16.'
    }

    return invalid
  }

  async function submit(event: FormEvent) {
    event.preventDefault()
    setError(undefined)

    const invalid = validate()
    setLocalErrors(invalid)
    if (Object.keys(invalid).length > 0) {
      // Report the bad fields without a request. Sending it would cost a round
      // trip and enqueue nothing.
      return
    }

    try {
      const vpc = await createVPC({
        name: name.trim(),
        cidr: cidr.trim(),
        mode,
        ...(tags.length > 0
          ? { tags: Object.fromEntries(tags.map((tag) => [tag.key, tag.value ?? ''])) }
          : {}),
      }).unwrap()

      dispatch(
        notify({
          type: 'in-progress',
          header: 'VPC creation started',
          content: 'The libvirt network is defined and started by a background task.',
        }),
      )
      navigate(`/vpcs/${vpc.id}`)
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
            description="A VPC is a private address block with its own libvirt network and Linux bridge. The network is brought up in the background."
          >
            Create VPC
          </Header>
        }
      >
        <Form
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button variant="link" onClick={() => navigate('/vpcs')} formAction="submit">
                Cancel
              </Button>
              <Button
                variant="primary"
                formAction="submit"
                loading={isCreating}
                loadingText="Creating"
                data-testid="submit-create-vpc"
              >
                Create VPC
              </Button>
            </SpaceBetween>
          }
        >
          <SpaceBetween size="l">
            {error !== undefined && Object.keys(errors).length === 0 && (
              <Alert type="error" header="The VPC was not created">
                {errorMessage(error, 'The VPC could not be created.')}
              </Alert>
            )}

            <Container header={<Header variant="h2">Address block</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="Name"
                  description="A unique label that seeds the DNS domain of the network."
                  errorText={errors['name']}
                  constraintText="1-63 characters"
                >
                  <Input
                    value={name}
                    onChange={({ detail }) => setName(detail.value)}
                    ariaLabel="Name"
                    placeholder="prod"
                    autoFocus
                  />
                </FormField>

                <FormField
                  label="IPv4 CIDR"
                  description="A private block, /16 to /28, that nothing else on this node already owns."
                  errorText={errors['cidr']}
                  constraintText="e.g. 10.0.0.0/16, 172.16.0.0/16"
                >
                  <Input
                    value={cidr}
                    onChange={({ detail }) => setCidr(detail.value)}
                    ariaLabel="IPv4 CIDR"
                    placeholder="10.0.0.0/16"
                  />
                </FormField>

                <FormField
                  label="Connectivity"
                  description="NAT lets guests reach the LAN; isolated keeps them on the bridge."
                  errorText={errors['mode']}
                >
                  <Select
                    selectedOption={
                      mode === 'nat'
                        ? { value: 'nat', label: 'Internet (NAT)' }
                        : { value: 'isolated', label: 'Isolated' }
                    }
                    onChange={({ detail }) => {
                      const value = detail.selectedOption?.value
                      if (value === 'nat' || value === 'isolated') {
                        setMode(value)
                      }
                    }}
                    options={[
                      { value: 'nat', label: 'Internet (NAT)' },
                      { value: 'isolated', label: 'Isolated' },
                    ]}
                    ariaLabel="Connectivity"
                  />
                </FormField>
              </SpaceBetween>
            </Container>

            <Container header={<Header variant="h2">Tags</Header>}>
              <FormField
                label="Tags"
                description="Optional key/value pairs to organize VPCs."
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
