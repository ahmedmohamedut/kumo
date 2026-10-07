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
import { useCreateSubnetMutation } from '@/features/subnets/api'
import { useListVPCsQuery } from '@/features/vpcs/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'

/** CIDR_RE accepts any dotted-quad/prefix pair; whether the block fits inside
 *  the VPC without overlapping a sibling is the API's call. */
const CIDR_RE = /^(\d{1,3}\.){3}\d{1,3}\/\d{1,2}$/

/**
 * CreateSubnetPage carves a subnet out of a VPC.
 *
 * Unlike a VPC, a subnet is only a row: the API answers `201` with the finished
 * resource, so the form navigates to the detail page as soon as it lands.
 * The CIDR must sit entirely inside its VPC and not overlap a sibling — the
 * API enforces both, and their answers arrive on the `cidr` field.
 */
export function CreateSubnetPage() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()

  const [vpcId, setVpcId] = useState('')
  const [name, setName] = useState('')
  const [cidr, setCidr] = useState('')
  const [tags, setTags] = useState<ReadonlyArray<TagEditorProps.Tag>>([])
  const [error, setError] = useState<ApiError>(undefined)
  const [localErrors, setLocalErrors] = useState<Record<string, string>>({})

  const { data: vpcsData } = useListVPCsQuery()
  const [createSubnet, { isLoading: isCreating }] = useCreateSubnetMutation()

  usePageMeta({
    breadcrumbs: [
      { text: 'Networking', href: '/' },
      { text: 'Subnets', href: '/subnets' },
      { text: 'Create subnet', href: '/subnets/create' },
    ],
  })

  const vpcs = vpcsData?.items ?? []
  const selectedVPC = vpcs.find((vpc) => vpc.id === vpcId)

  // Local validation wins over the API's: a form that is fixed and resubmitted
  // must not keep showing the complaint of the previous attempt.
  const errors = { ...fieldErrors(error), ...localErrors }

  function validate(): Record<string, string> {
    const invalid: Record<string, string> = {}

    if (vpcId === '') {
      invalid['vpc_id'] = 'Choose the VPC this subnet belongs to.'
    }

    if (name.trim() === '') {
      invalid['name'] = 'Enter a name for the subnet.'
    } else if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name.trim()) || name.trim().length > 63) {
      invalid['name'] =
        'Must be 1-63 characters of lowercase letters, digits and dashes, starting and ending with a letter or digit.'
    }

    if (cidr.trim() === '') {
      invalid['cidr'] = 'Enter an IPv4 CIDR block for the subnet.'
    } else if (!CIDR_RE.test(cidr.trim())) {
      invalid['cidr'] = 'Enter a CIDR block like 10.0.1.0/24.'
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
      const subnet = await createSubnet({
        vpc_id: vpcId,
        name: name.trim(),
        cidr: cidr.trim(),
        ...(tags.length > 0
          ? { tags: Object.fromEntries(tags.map((tag) => [tag.key, tag.value ?? ''])) }
          : {}),
      }).unwrap()

      dispatch(
        notify({
          type: 'success',
          header: 'Subnet created',
          content: `Subnet ${subnet.id} is ready to take addresses.`,
        }),
      )
      navigate(`/subnets/${subnet.id}`)
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
            description="A subnet is a slice of its VPC's address block that instances attach to. It is ready as soon as it is created."
          >
            Create subnet
          </Header>
        }
      >
        <Form
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button variant="link" onClick={() => navigate('/subnets')} formAction="submit">
                Cancel
              </Button>
              <Button
                variant="primary"
                formAction="submit"
                loading={isCreating}
                loadingText="Creating"
                data-testid="submit-create-subnet"
              >
                Create subnet
              </Button>
            </SpaceBetween>
          }
        >
          <SpaceBetween size="l">
            {error !== undefined && Object.keys(errors).length === 0 && (
              <Alert type="error" header="The subnet was not created">
                {errorMessage(error, 'The subnet could not be created.')}
              </Alert>
            )}

            <Container header={<Header variant="h2">Subnet details</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="VPC"
                  description="The VPC that owns the gateway of this subnet."
                  errorText={errors['vpc_id']}
                >
                  <Select
                    selectedOption={
                      selectedVPC === undefined
                        ? null
                        : {
                            value: selectedVPC.id,
                            label: selectedVPC.name,
                            description: selectedVPC.cidr,
                          }
                    }
                    onChange={({ detail }) => setVpcId(detail.selectedOption?.value ?? '')}
                    options={vpcs.map((vpc) => ({
                      value: vpc.id,
                      label: vpc.name,
                      description: vpc.cidr,
                    }))}
                    placeholder="Choose a VPC"
                    ariaLabel="VPC"
                  />
                </FormField>

                <FormField
                  label="Name"
                  description="A name unique within the VPC."
                  errorText={errors['name']}
                  constraintText="1-63 characters"
                >
                  <Input
                    value={name}
                    onChange={({ detail }) => setName(detail.value)}
                    ariaLabel="Name"
                    placeholder="db"
                  />
                </FormField>

                <FormField
                  label="IPv4 CIDR"
                  description={
                    selectedVPC === undefined
                      ? 'Must sit entirely inside the chosen VPC and not overlap a sibling.'
                      : `Must sit entirely inside ${selectedVPC.name} (${selectedVPC.cidr}) and not overlap a sibling.`
                  }
                  errorText={errors['cidr']}
                  constraintText="e.g. 10.0.1.0/24"
                >
                  <Input
                    value={cidr}
                    onChange={({ detail }) => setCidr(detail.value)}
                    ariaLabel="IPv4 CIDR"
                    placeholder="10.0.1.0/24"
                  />
                </FormField>
              </SpaceBetween>
            </Container>

            <Container header={<Header variant="h2">Tags</Header>}>
              <FormField
                label="Tags"
                description="Optional key/value pairs to organize subnets."
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
