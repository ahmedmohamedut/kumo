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
import Select from '@cloudscape-design/components/select'
import SpaceBetween from '@cloudscape-design/components/space-between'
import Spinner from '@cloudscape-design/components/spinner'

import { usePageMeta } from '@/app/pageMeta'
import { useAppDispatch } from '@/app/store'
import { useImportImageMutation, useListCatalogImagesQuery } from '@/features/images/api'
import { notify } from '@/features/notifications/notificationsSlice'
import { errorMessage, fieldErrors, type ApiError } from '@/lib/apiError'

type SourceType = 'url' | 'catalog'
type Architecture = 'x86_64' | 'aarch64'

/** MAX_DISK_GIB is the largest root disk the API accepts, in GiB. */
const MAX_DISK_GIB = 65536

/** SOURCE_OPTIONS is where the URL for a new image comes from. */
const SOURCE_OPTIONS = [
  {
    value: 'url',
    label: 'Public URL',
    description: 'A qcow2 or raw image served over HTTP(S).',
  },
  {
    value: 'catalog',
    label: 'Well-known image',
    description: 'An image from the Kumo catalog.',
  },
]

/** ARCHITECTURE_OPTIONS is the architecture of the image file itself. */
const ARCHITECTURE_OPTIONS = [
  { value: 'x86_64', label: 'x86_64' },
  { value: 'aarch64', label: 'arm64 (aarch64)' },
]

/** CHECKSUM_OPTIONS are the digests the importer knows how to verify. */
const CHECKSUM_OPTIONS = [
  { value: 'sha256', label: 'SHA-256' },
  { value: 'sha512', label: 'SHA-512' },
]

/** CHECKSUM_HEX_LENGTH is how many hex characters each algorithm produces. */
const CHECKSUM_HEX_LENGTH: Record<'sha256' | 'sha512', number> = { sha256: 64, sha512: 128 }

/**
 * ImportImagePage imports an image from a URL, or from the catalog of well-known
 * cloud images.
 *
 * The submit does not wait for the download: the API answers `202` with the new
 * image in `pending` and the id of the task doing the work, and the page goes
 * straight to that image so the operator can watch it land
 * (03-frontend.md §6.4). A form that sat on a spinner for the length of a 600 MB
 * download would look broken and would hold the browser hostage to a link that
 * may be slow for reasons that are not Kumo's fault.
 *
 * Picking a catalog entry fills the detail fields from it. The catalog already
 * knows the OS, the architecture and the URL of an image Kumo can fetch, so
 * asking the operator to retype what this node already knows invites a typo that
 * only surfaces when an instance fails to boot from it.
 */
export function ImportImagePage() {
  const dispatch = useAppDispatch()
  const navigate = useNavigate()

  const [sourceType, setSourceType] = useState<SourceType>('url')
  const [url, setUrl] = useState('')
  const [catalogId, setCatalogId] = useState('')
  const [name, setName] = useState('')
  const [osFamily, setOsFamily] = useState('linux')
  const [osVersion, setOsVersion] = useState('')
  const [architecture, setArchitecture] = useState<Architecture>('x86_64')
  const [defaultUser, setDefaultUser] = useState('')
  const [checksumAlgorithm, setChecksumAlgorithm] = useState<'sha256' | 'sha512'>('sha256')
  const [checksumValue, setChecksumValue] = useState('')
  const [minDiskGb, setMinDiskGb] = useState('')
  const [error, setError] = useState<ApiError>(undefined)
  const [localErrors, setLocalErrors] = useState<Record<string, string>>({})

  const { data: catalog, isLoading: isLoadingCatalog } = useListCatalogImagesQuery()
  const [importImage, { isLoading: isImporting }] = useImportImageMutation()

  usePageMeta({
    breadcrumbs: [
      { text: 'Compute', href: '/' },
      { text: 'Images', href: '/images' },
      { text: 'Import image', href: '/images/import' },
    ],
  })

  const catalogImages = catalog ?? []
  const selectedCatalogImage = catalogImages.find((item) => item.id === catalogId)
  const catalogOptions = catalogImages.map((item) => ({
    value: item.id,
    label: item.name,
    description: [item.os_family, item.os_version, item.architecture]
      .filter((part) => part !== undefined)
      .join(' · '),
  }))
  // Local validation wins over the API's: a form that is fixed and resubmitted
  // must not keep showing the complaint of the previous attempt.
  const errors = { ...fieldErrors(error), ...localErrors }

  function selectCatalogImage(id: string) {
    setCatalogId(id)
    const item = catalogImages.find((candidate) => candidate.id === id)
    if (item === undefined) {
      return
    }
    setUrl(item.source_url)
    // The catalog's `name` is a label for a picker ("Debian 12 (bookworm)"), and
    // an image name may not contain spaces or parentheses, so the id ("debian-12")
    // is what becomes the name. The operator can still rename it.
    if (item.id !== '') {
      setName(item.id)
    }
    if (item.os_family !== undefined) {
      setOsFamily(item.os_family)
    }
    if (item.os_version !== undefined) {
      setOsVersion(item.os_version)
    }
    if (item.architecture !== undefined) {
      setArchitecture(item.architecture)
    }
    if (item.default_user !== undefined) {
      setDefaultUser(item.default_user)
    }
    if (item.min_disk_gb !== undefined) {
      setMinDiskGb(String(item.min_disk_gb))
    }
  }

  function validate(): Record<string, string> {
    const invalid: Record<string, string> = {}

    if (name.trim() === '') {
      invalid['name'] = 'Enter a name for the image.'
    } else if (name.trim().length > 128) {
      invalid['name'] = 'The name can be at most 128 characters.'
    }

    if (sourceType === 'catalog' && selectedCatalogImage === undefined) {
      invalid['source_url'] = 'Choose an image to import.'
    } else if (!/^https?:\/\//i.test(url.trim())) {
      invalid['source_url'] = 'The URL must start with http:// or https://.'
    }

    if (osFamily.trim() === '') {
      invalid['os_family'] = 'Enter the operating system family, e.g. ubuntu or linux.'
    }

    const disk = minDiskGb.trim()
    if (disk !== '') {
      const parsed = Number(disk)
      if (!Number.isInteger(parsed) || parsed < 1 || parsed > MAX_DISK_GIB) {
        invalid['min_disk_gb'] = `Enter a whole number of GiB between 1 and ${MAX_DISK_GIB}.`
      }
    }

    const digest = checksumValue.trim().toLowerCase()
    if (digest !== '' && !/^[0-9a-f]+$/.test(digest)) {
      invalid['checksum'] = 'A checksum is hexadecimal; paste only the digest.'
    } else if (digest !== '' && digest.length !== CHECKSUM_HEX_LENGTH[checksumAlgorithm]) {
      invalid['checksum'] =
        `A ${checksumAlgorithm.toUpperCase()} checksum is ${CHECKSUM_HEX_LENGTH[checksumAlgorithm]} hex characters, this one is ${digest.length}.`
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

    const disk = minDiskGb.trim()

    try {
      const image = await importImage({
        name: name.trim(),
        source_url: url.trim(),
        os_family: osFamily.trim(),
        architecture,
        ...(osVersion.trim() === '' ? {} : { os_version: osVersion.trim() }),
        ...(defaultUser.trim() === '' ? {} : { default_user: defaultUser.trim() }),
        ...(disk === '' ? {} : { min_disk_gb: Number(disk) }),
        ...(checksumValue.trim() === ''
          ? {}
          : { checksum: { algorithm: checksumAlgorithm, value: checksumValue.trim() } }),
      }).unwrap()

      dispatch(
        notify({
          type: 'success',
          header: 'Import started',
          content: `${image.name} is being imported. The download runs in the background.`,
        }),
      )
      navigate(`/images/${image.id}`)
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
            description="Kumo downloads the image once, checks that it is a qcow2 or raw disk, and keeps the copy on this node. Every instance launched from it gets its own root disk."
          >
            Import image
          </Header>
        }
      >
        <Form
          actions={
            <SpaceBetween direction="horizontal" size="s">
              <Button variant="link" onClick={() => navigate('/images')} formAction="submit">
                Cancel
              </Button>
              <Button
                variant="primary"
                formAction="submit"
                loading={isImporting}
                loadingText="Importing"
                data-testid="submit-import"
              >
                Import image
              </Button>
            </SpaceBetween>
          }
        >
          <SpaceBetween size="l">
            {error !== undefined && Object.keys(errors).length === 0 && (
              <Alert type="error" header="The import was not started">
                {errorMessage(error, 'The image could not be imported.')}
              </Alert>
            )}

            <Container header={<Header variant="h2">Source</Header>}>
              <SpaceBetween size="m">
                <FormField label="Source" description="Where Kumo downloads the image file from.">
                  <Select
                    // The whole option is handed back, not just its value: the
                    // trigger renders what it is given, and `{value}` alone would
                    // show the operator "url" instead of "Public URL".
                    selectedOption={
                      SOURCE_OPTIONS.find((option) => option.value === sourceType) ?? null
                    }
                    onChange={({ detail }) =>
                      setSourceType(detail.selectedOption?.value as SourceType)
                    }
                    options={SOURCE_OPTIONS}
                    ariaLabel="Source"
                  />
                </FormField>

                {sourceType === 'url' ? (
                  <FormField
                    label="Image URL"
                    description="A direct link to a qcow2 or raw image. Private and metadata addresses are refused."
                    errorText={errors['source_url']}
                  >
                    <Input
                      value={url}
                      onChange={({ detail }) => setUrl(detail.value)}
                      ariaLabel="Image URL"
                      placeholder="https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img"
                      data-testid="import-url"
                    />
                  </FormField>
                ) : isLoadingCatalog ? (
                  <Box>
                    <Spinner /> Loading the catalog
                  </Box>
                ) : catalogImages.length === 0 ? (
                  <Alert type="info" header="The catalog is empty">
                    This node offers no well-known images. Import from a URL instead.
                  </Alert>
                ) : (
                  <FormField
                    label="Well-known image"
                    description="Choosing one fills in its URL, name, operating system and minimum disk size."
                    errorText={errors['source_url']}
                  >
                    <Select
                      // The selection has to be one of the options: a catalog
                      // entry has no `label`, so the trigger rendered blank.
                      selectedOption={
                        catalogOptions.find((option) => option.value === catalogId) ?? null
                      }
                      onChange={({ detail }) => {
                        const value = detail.selectedOption?.value
                        if (value !== undefined && value !== '') {
                          selectCatalogImage(value)
                        }
                      }}
                      options={catalogOptions}
                      placeholder="Choose an image"
                      ariaLabel="Well-known image"
                      data-testid="import-catalog"
                    />
                  </FormField>
                )}
              </SpaceBetween>
            </Container>

            <Container header={<Header variant="h2">Details</Header>}>
              <SpaceBetween size="m">
                <FormField
                  label="Name"
                  description="Letters, digits, '.', '_' and '-'. The name must be unique on this node."
                  errorText={errors['name']}
                  constraintText="1-128 characters"
                >
                  <Input
                    value={name}
                    onChange={({ detail }) => setName(detail.value)}
                    ariaLabel="Name"
                    placeholder="ubuntu-22.04"
                    autoFocus
                    data-testid="import-name"
                  />
                </FormField>

                <FormField
                  label="Operating system family"
                  description="e.g. ubuntu, debian, rocky. Imported images are private to this node."
                  errorText={errors['os_family']}
                >
                  <Input
                    value={osFamily}
                    onChange={({ detail }) => setOsFamily(detail.value)}
                    ariaLabel="Operating system family"
                    placeholder="ubuntu"
                  />
                </FormField>

                <FormField label="Operating system version" errorText={errors['os_version']}>
                  <Input
                    value={osVersion}
                    onChange={({ detail }) => setOsVersion(detail.value)}
                    ariaLabel="Operating system version"
                    placeholder="22.04"
                  />
                </FormField>

                <FormField
                  label="Architecture"
                  description="The architecture of the image file. It must match what the instances on this node run."
                  errorText={errors['architecture']}
                >
                  <Select
                    selectedOption={
                      ARCHITECTURE_OPTIONS.find((option) => option.value === architecture) ?? null
                    }
                    onChange={({ detail }) =>
                      setArchitecture(detail.selectedOption?.value as Architecture)
                    }
                    options={ARCHITECTURE_OPTIONS}
                    ariaLabel="Architecture"
                  />
                </FormField>

                <FormField
                  label="Default user"
                  description="Optional login name an instance launched from this image gets."
                  errorText={errors['default_user']}
                >
                  <Input
                    value={defaultUser}
                    onChange={({ detail }) => setDefaultUser(detail.value)}
                    ariaLabel="Default user"
                    placeholder="ubuntu"
                  />
                </FormField>

                <FormField
                  label="Checksum"
                  description="Optional. When given, the downloaded file must hash to exactly this value, or the import fails. A catalog entry links to the vendor's SHA256SUMS to copy it from."
                  errorText={errors['checksum']}
                >
                  <SpaceBetween size="s">
                    <Select
                      selectedOption={
                        CHECKSUM_OPTIONS.find((option) => option.value === checksumAlgorithm) ??
                        null
                      }
                      onChange={({ detail }) =>
                        setChecksumAlgorithm(detail.selectedOption?.value as 'sha256' | 'sha512')
                      }
                      options={CHECKSUM_OPTIONS}
                      ariaLabel="Checksum algorithm"
                    />
                    <Input
                      value={checksumValue}
                      onChange={({ detail }) => setChecksumValue(detail.value)}
                      ariaLabel="Checksum value"
                      placeholder={
                        checksumAlgorithm === 'sha256' ? '64 hex characters' : '128 hex characters'
                      }
                    />
                  </SpaceBetween>
                </FormField>

                <FormField
                  label="Minimum disk size (GiB)"
                  description="The smallest root disk an instance launched from this image can boot from."
                  errorText={errors['min_disk_gb']}
                >
                  <Input
                    value={minDiskGb}
                    onChange={({ detail }) => setMinDiskGb(detail.value)}
                    ariaLabel="Minimum disk size"
                    placeholder="8"
                  />
                </FormField>
              </SpaceBetween>
            </Container>

            <Alert type="info" header="This downloads a whole file">
              The file is verified as a qcow2 or raw disk before the image is offered to instances.
              Nothing is published until the download completes, so an interrupted transfer leaves
              nothing half-finished to clean up.
            </Alert>
          </SpaceBetween>
        </Form>
      </ContentLayout>
    </form>
  )
}
