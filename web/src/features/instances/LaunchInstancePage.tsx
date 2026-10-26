import { useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import Alert from '@cloudscape-design/components/alert'
import Box from '@cloudscape-design/components/box'
import Button from '@cloudscape-design/components/button'
import Container from '@cloudscape-design/components/container'
import ContentLayout from '@cloudscape-design/components/content-layout'
import Form from '@cloudscape-design/components/form'
import FormField from '@cloudscape-design/components/form-field'
import Header from '@cloudscape-design/components/header'
import Input from '@cloudscape-design/components/input'
import KeyValuePairs from '@cloudscape-design/components/key-value-pairs'
import Link from '@cloudscape-design/components/link'
import Select from '@cloudscape-design/components/select'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'
import TagEditor, { type TagEditorProps } from '@cloudscape-design/components/tag-editor'
import Textarea from '@cloudscape-design/components/textarea'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch } from '@/app/store'
import { useLaunchInstanceMutation } from '@/features/instances/api'
import { useListInstanceTypesQuery } from '@/features/instancetypes/api'
import { useListImagesQuery } from '@/features/images/api'
import { useListKeyPairsQuery } from '@/features/keypairs/api'
import { useListSubnetsQuery } from '@/features/subnets/api'
import { useListVPCsQuery } from '@/features/vpcs/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { formatGiB } from '@/lib/format'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'

/** MAX_DISK_GIB is the largest root disk the API accepts, in GiB. */
const MAX_DISK_GIB = 2048

/** MAX_USER_DATA_BYTES is the API's 16 KiB cap on cloud-init, minus the newline. */
const MAX_USER_DATA_BYTES = 16 * 1024

/** NO_KEY_PAIR is the "launch without SSH access" sentinel of the key pair picker. */
const NO_KEY_PAIR = '__none__'

/**
 * LaunchInstancePage launches an instance from an image on this node
 * (03-frontend.md §6.2).
 *
 * The submit does not wait for the boot: the API answers `202` with the instance
 * in `pending` and a task id, and the page goes straight to the detail page so the
 * operator watches it come up. A form sitting on a spinner until the guest is
 * reachable would look broken and would hold the browser hostage to a boot that
 * may take minutes for reasons that are not the browser's business.
 *
 * The root disk is pre-filled from the selected image's `min_disk_gb` and cannot be
 * set below it, because that is the rule the API enforces anyway — catching it
 * here saves a round trip and a red field.
 */
export function LaunchInstancePage() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()

  const [name, setName] = useState('')
  const [imageId, setImageId] = useState('')
  const [instanceType, setInstanceType] = useState('')
  const [keyPairId, setKeyPairId] = useState(NO_KEY_PAIR)
  const [rootDiskGb, setRootDiskGb] = useState('')
  const [userData, setUserData] = useState('')
  const [tags, setTags] = useState<ReadonlyArray<TagEditorProps.Tag>>([])
  const [vpcId, setVpcId] = useState('')
  const [subnetId, setSubnetId] = useState('')
  const [error, setError] = useState<ApiError>(undefined)
  const [localErrors, setLocalErrors] = useState<Record<string, string>>({})

  // Only `available` images can launch, so the list is filtered here rather than
  // offering a picker full of entries that would answer 422.
  const { data: images, isLoading: isLoadingImages } = useListImagesQuery({ state: 'available' })
  const { data: types, isLoading: isLoadingTypes } = useListInstanceTypesQuery()
  const { data: keyPairs } = useListKeyPairsQuery()
  const { data: vpcsData, isLoading: isLoadingVpcs } = useListVPCsQuery()
  const [launchInstance, { isLoading: isLaunching }] = useLaunchInstanceMutation()

  const vpcs = vpcsData?.items ?? []
  const defaultVPC = vpcs.find((vpc) => vpc.is_default)

  // The pickers start on the defaults the node bootstrapped, when those are
  // unique — an operator who mostly launches into the default VPC should not
  // have to click through two dropdowns every time. The defaults are derived,
  // not stored: state keeps only the operator's explicit choices, so picking a
  // VPC automatically re-picks its default subnet without syncing state in an
  // effect, and an operator who changes their mind gets the defaults back
  // without anything to clear.
  const effectiveVpcId = vpcId === '' ? (defaultVPC?.id ?? '') : vpcId
  const selectedVPC = vpcs.find((vpc) => vpc.id === effectiveVpcId)
  const { data: subnetsData } = useListSubnetsQuery(
    { vpcId: effectiveVpcId },
    { skip: effectiveVpcId === '' },
  )
  const subnetOptions = subnetsData?.items ?? []
  const defaultSubnet = subnetOptions.find((subnet) => subnet.is_default)
  const effectiveSubnetId = subnetId === '' ? (defaultSubnet?.id ?? '') : subnetId
  const selectedSubnet = subnetOptions.find((subnet) => subnet.id === effectiveSubnetId)

  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Instances', href: '/instances' },
      { text: 'Launch instance', href: '/instances/launch' },
    ],
  })

  const availableImages = images?.items ?? []
  const presets = types ?? []
  const selectedImage = availableImages.find((image) => image.id === imageId)
  const selectedType = presets.find((preset) => preset.id === instanceType)
  const selectedKeyPair = keyPairs?.items.find((pair) => pair.id === keyPairId)

  // Local validation wins over the API's: a form that is fixed and resubmitted
  // must not keep showing the complaint of the previous attempt.
  const errors = { ...fieldErrors(error), ...localErrors }

  /** minDiskGb is the smallest root disk this image can boot from. */
  const minDiskGb = selectedImage?.min_disk_gb ?? 1

  function selectImage(id: string) {
    setImageId(id)
    const image = availableImages.find((candidate) => candidate.id === id)
    // Re-seed the disk from the new image. Carrying the old image's number over
    // would either be below this image's floor or waste space the operator did not
    // ask for, and either way it is a value they did not choose.
    if (image !== undefined) {
      setRootDiskGb(String(image.min_disk_gb))
    }
  }

  function validate(): Record<string, string> {
    const invalid: Record<string, string> = {}

    if (name.trim() === '') {
      invalid['name'] = 'Enter a name for the instance.'
    } else if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name.trim())) {
      // The API's rule: an RFC 1123 label, 1-63 characters. Stated here so the
      // red field arrives before the round trip rather than after it.
      invalid['name'] =
        'Must be 1-63 characters of lowercase letters, digits and dashes, starting and ending with a letter or digit.'
    }

    if (imageId === '') {
      invalid['image_id'] = 'Choose an image to launch from.'
    }

    if (instanceType === '') {
      invalid['instance_type'] = 'Choose an instance type.'
    }

    // A VPC without a subnet would be a lie to submit: the API has no `vpc_id`
    // field, so leaving the subnet empty inside a chosen VPC would silently
    // attach the instance to the default subnet of the default VPC instead.
    if (effectiveVpcId !== '' && effectiveSubnetId === '') {
      invalid['subnet_id'] = 'Choose a subnet in the selected VPC.'
    }

    const disk = rootDiskGb.trim()
    if (disk === '') {
      invalid['root_disk_gb'] = 'Enter a root disk size in GiB.'
    } else {
      const parsed = Number(disk)
      if (!Number.isInteger(parsed)) {
        invalid['root_disk_gb'] = 'Enter a whole number of GiB.'
      } else if (parsed < minDiskGb) {
        invalid['root_disk_gb'] =
          `${selectedImage?.name ?? 'This image'} needs at least ${formatGiB(minDiskGb)}.`
      } else if (parsed > MAX_DISK_GIB) {
        invalid['root_disk_gb'] = `The root disk can be at most ${formatGiB(MAX_DISK_GIB)}.`
      }
    }

    if (new TextEncoder().encode(userData).length > MAX_USER_DATA_BYTES) {
      invalid['user_data'] = 'User data can be at most 16 KiB.'
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
      // trip and enqueue nothing, so the operator would see the form complain
      // about a failure that never happened on the node.
      return
    }

    try {
      const instance = await launchInstance({
        name: name.trim(),
        image_id: imageId,
        instance_type: instanceType,
        ...(keyPairId === NO_KEY_PAIR ? {} : { key_pair_id: keyPairId }),
        // Left out entirely when no subnet was resolved, which makes the API
        // attach the launch to the default subnet of the default VPC.
        ...(effectiveSubnetId === '' ? {} : { subnet_id: effectiveSubnetId }),
        root_disk_gb: Number(rootDiskGb.trim()),
        ...(userData.trim() === '' ? {} : { user_data: userData }),
        // The contract carries `count` for a release that can launch several
        // instances from one image. It is refused above 1 today, so it is sent as 1
        // rather than omitted, which would fail the required-field check.
        count: 1,
        ...(tags.length > 0
          ? { tags: Object.fromEntries(tags.map((tag) => [tag.key, tag.value ?? ''])) }
          : {}),
      }).unwrap()

      dispatch(
        notify({
          type: 'success',
          header: 'Instance launched',
          content: `Instance ${instance.id} is being launched. The boot runs in the background.`,
        }),
      )
      navigate(`/instances/${instance.id}`)
    } catch (err) {
      setError(err as ApiError)
    }
  }

  const isLoadingLookups = isLoadingImages || isLoadingTypes || isLoadingVpcs

  return (
    <form onSubmit={submit}>
      <ContentLayout
        header={
          <Header
            variant="h1"
            description="Kumo copies the image into a root disk for this instance, injects the key pair and boots it. The launch runs as a task in the background."
          >
            Launch instance
          </Header>
        }
      >
        <Form
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button variant="link" onClick={() => navigate('/instances')} formAction="submit">
                Cancel
              </Button>
              <Button
                variant="primary"
                formAction="submit"
                loading={isLaunching}
                loadingText="Launching"
                data-testid="submit-launch"
              >
                Launch instance
              </Button>
            </SpaceBetween>
          }
        >
          <SpaceBetween size="l">
            {error !== undefined && Object.keys(errors).length === 0 && (
              <Alert type="error" header="The instance was not launched">
                {errorMessage(error, 'The instance could not be launched.')}
              </Alert>
            )}

            <Container header={<Header variant="h2">Name and tags</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="Name"
                  description="Lowercase letters, digits and dashes. The name is reused after an instance is terminated."
                  errorText={errors['name']}
                  constraintText="1-63 characters"
                >
                  <Input
                    value={name}
                    onChange={({ detail }) => setName(detail.value)}
                    ariaLabel="Name"
                    placeholder="web-1"
                    autoFocus
                  />
                </FormField>

                <FormField
                  label="Tags"
                  description="Optional key/value pairs to organize instances."
                  errorText={errors['tags']}
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

            {isLoadingLookups ? (
              <Box>
                <Spinner /> Loading images and instance types
              </Box>
            ) : (
              <>
                <Container header={<Header variant="h2">Image</Header>}>
                  {availableImages.length === 0 ? (
                    <Alert type="info" header="No images on this node">
                      Import an image before launching an instance.
                    </Alert>
                  ) : (
                    <FormField
                      label="Image"
                      description="Only images that finished importing can be launched from."
                      errorText={errors['image_id']}
                    >
                      <Select
                        selectedOption={
                          // Rebuilt as an option rather than passed through: an
                          // Image carries `tags`, which collides with the
                          // OptionDefinition of the same name and is not what the
                          // trigger renders anyway.
                          selectedImage === undefined
                            ? null
                            : {
                                value: selectedImage.id,
                                label: selectedImage.name,
                                description: [
                                  [selectedImage.os_family, selectedImage.os_version]
                                    .filter(Boolean)
                                    .join(' '),
                                  `min disk ${formatGiB(selectedImage.min_disk_gb ?? 1)}`,
                                ]
                                  .filter(Boolean)
                                  .join(' · '),
                              }
                        }
                        onChange={({ detail }) => {
                          const value = detail.selectedOption?.value
                          if (value !== undefined && value !== '') {
                            selectImage(value)
                          }
                        }}
                        options={availableImages.map((image) => ({
                          value: image.id,
                          label: image.name,
                          description: [
                            [image.os_family, image.os_version].filter(Boolean).join(' ') ||
                              image.source_url,
                            `${image.architecture === 'aarch64' ? 'arm64' : 'x86_64'}`,
                            `min disk ${formatGiB(image.min_disk_gb)}`,
                          ].join(' · '),
                        }))}
                        placeholder="Choose an image"
                        ariaLabel="Image"
                      />
                    </FormField>
                  )}
                </Container>

                <Container header={<Header variant="h2">Instance type</Header>}>
                  <FormField
                    label="Instance type"
                    description="Presets for how much CPU and memory the instance gets."
                    errorText={errors['instance_type']}
                  >
                    {(() => {
                      const options = presets.map((preset) => ({
                        value: preset.id,
                        label: preset.id,
                        description:
                          preset.description ??
                          `${preset.vcpus} vCPU · ${formatGiB(
                            Math.round(preset.memory_mib / 1024),
                          )}`,
                      }))
                      const selected = options.find((o) => o.value === instanceType)

                      return (
                        <Select
                          selectedOption={selected ?? null}
                          onChange={({ detail }) =>
                            setInstanceType(detail.selectedOption?.value ?? '')
                          }
                          options={options}
                          placeholder="Choose an instance type"
                          ariaLabel="Instance type"
                        />
                      )
                    })()}
                  </FormField>
                </Container>

                <Container header={<Header variant="h2">Network settings</Header>}>
                  <SpaceBetween size="m">
                    <FormField
                      label="VPC"
                      description="The private address block the instance joins."
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
                        onChange={({ detail }) => {
                          // A new VPC invalidates the subnet choice: the old one
                          // belongs to the old VPC and would be refused anyway.
                          setVpcId(detail.selectedOption?.value ?? '')
                          setSubnetId('')
                        }}
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
                      label="Subnet"
                      description={
                        selectedVPC === undefined
                          ? 'Choose a VPC first; the subnets partitioned out of it appear here.'
                          : `The address slice of ${selectedVPC.name}. Omitted, the launch goes to the default subnet.`
                      }
                      errorText={errors['subnet_id']}
                    >
                      <Select
                        selectedOption={
                          selectedSubnet === undefined
                            ? null
                            : {
                                value: selectedSubnet.id,
                                label: selectedSubnet.name,
                                description: `${selectedSubnet.cidr} · ${
                                  selectedSubnet.usable_ips - selectedSubnet.allocated_ips
                                } IPs available`,
                              }
                        }
                        onChange={({ detail }) => setSubnetId(detail.selectedOption?.value ?? '')}
                        options={subnetOptions.map((subnet) => ({
                          value: subnet.id,
                          label: subnet.name,
                          description: `${subnet.cidr} · ${
                            subnet.usable_ips - subnet.allocated_ips
                          } IPs available`,
                        }))}
                        placeholder={
                          selectedVPC === undefined ? 'Choose a VPC first' : 'Choose a subnet'
                        }
                        disabled={effectiveVpcId === ''}
                        ariaLabel="Subnet"
                      />
                    </FormField>
                  </SpaceBetween>
                </Container>

                <Container header={<Header variant="h2">Key pair</Header>}>
                  <FormField
                    label="Key pair"
                    description="The public key is injected into the guest so you can log in as its default user."
                    errorText={errors['key_pair_id']}
                  >
                    <SpaceBetween size="xs">
                      <Select
                        selectedOption={
                          keyPairId === NO_KEY_PAIR
                            ? { value: NO_KEY_PAIR, label: 'Proceed without a key pair' }
                            : selectedKeyPair === undefined
                              ? null
                              : {
                                  value: selectedKeyPair.id,
                                  label: selectedKeyPair.name,
                                  description: selectedKeyPair.type === 'rsa' ? 'RSA' : 'ED25519',
                                }
                        }
                        onChange={({ detail }) =>
                          setKeyPairId(detail.selectedOption?.value ?? NO_KEY_PAIR)
                        }
                        options={[
                          { value: NO_KEY_PAIR, label: 'Proceed without a key pair' },
                          ...(keyPairs?.items ?? []).map((pair) => ({
                            value: pair.id,
                            label: pair.name,
                            description: pair.type === 'rsa' ? 'RSA' : 'ED25519',
                          })),
                        ]}
                        ariaLabel="Key pair"
                      />
                      <Box variant="small">
                        <Link href="/key-pairs/create" external>
                          Create a new key pair
                        </Link>
                      </Box>
                    </SpaceBetween>
                  </FormField>
                </Container>

                <Container header={<Header variant="h2">Storage</Header>}>
                  <FormField
                    label="Root disk (GiB)"
                    description={
                      selectedImage === undefined
                        ? 'Choose an image first; its minimum disk size is filled in.'
                        : `The smallest root disk ${selectedImage.name} can boot from is ${formatGiB(minDiskGb)}.`
                    }
                    errorText={errors['root_disk_gb']}
                  >
                    <Input
                      type="number"
                      value={rootDiskGb}
                      onChange={({ detail }) => setRootDiskGb(detail.value)}
                      ariaLabel="Root disk"
                      placeholder={String(minDiskGb)}
                    />
                  </FormField>
                </Container>

                <Container header={<Header variant="h2">Advanced details</Header>}>
                  <FormField
                    label="User data (cloud-init)"
                    description="Optional cloud-init payload, at most 16 KiB. Kumo never logs it."
                    errorText={errors['user_data']}
                  >
                    <Textarea
                      value={userData}
                      onChange={({ detail }) => setUserData(detail.value)}
                      ariaLabel="User data"
                      placeholder={'#cloud-config\npackages:\n  - nginx\n'}
                      rows={6}
                    />
                  </FormField>
                </Container>
              </>
            )}

            <Container header={<Header variant="h2">Summary</Header>}>
              {/* The summary restates the form so an operator can check what they
                  are about to create without scrolling back up. A dash marks a
                  choice not made yet, which is different from a default of
                  nothing — the two must not look alike. */}
              <KeyValuePairs
                columns={2}
                ariaLabel="Launch summary"
                items={[
                  { label: 'Name', value: name.trim() === '' ? '—' : name.trim() },
                  { label: 'Image', value: selectedImage?.name ?? '—' },
                  { label: 'Instance type', value: selectedType?.id ?? '—' },
                  {
                    label: 'VPC',
                    value: selectedVPC?.name ?? '—',
                  },
                  {
                    label: 'Subnet',
                    value: selectedSubnet?.name ?? '—',
                  },
                  {
                    label: 'Key pair',
                    value: keyPairId === NO_KEY_PAIR ? 'None' : (selectedKeyPair?.name ?? '—'),
                  },
                  {
                    label: 'Root disk',
                    value: rootDiskGb.trim() === '' ? '—' : formatGiB(Number(rootDiskGb.trim())),
                  },
                ]}
              />
            </Container>
          </SpaceBetween>
        </Form>
      </ContentLayout>
    </form>
  )
}
