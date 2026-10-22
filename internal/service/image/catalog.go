package image

// catalogImages is the curated list the console offers before the user pastes
// any URL of their own. It is static rather than fetched: Kumo does not mirror
// distribution metadata, and a catalog that changed on every request would make
// the "import an image" form untestable.
//
// The URLs point at the vendor's stable release paths and are the same ones a
// user would paste by hand; ChecksumURL publishes where the vendor's hash can be
// checked. The import workflow verifies a checksum only when the request carries
// one, so a catalog entry without ChecksumURL imports unverified, which is
// exactly what pasting the URL by hand does.
//
// OSFamily names the distribution, not the kernel: it is the same vocabulary as
// Image.OSFamily ("ubuntu", "debian", "rocky"), so an image imported from here
// carries the same value as one imported from a pasted URL, and the console can
// filter on it. It is the default the import form offers, not a value the
// operator has to keep.
var catalogImages = []CatalogImage{
	{
		ID:           "ubuntu-2404",
		Name:         "Ubuntu 24.04 LTS",
		Description:  "Canonical's long term support release, server image",
		OSFamily:     "ubuntu",
		OSVersion:    "24.04",
		Architecture: ArchX86_64,
		DefaultUser:  "ubuntu",
		MinDiskGB:    8,
		SourceURL:    "https://cloud-images.ubuntu.com/releases/noble/release/ubuntu-24.04-server-cloudimg-amd64.img",
		ChecksumURL:  "https://cloud-images.ubuntu.com/releases/noble/release/SHA256SUMS",
	},
	{
		ID:           "ubuntu-2204",
		Name:         "Ubuntu 22.04 LTS",
		Description:  "Canonical's previous long term support release, server image",
		OSFamily:     "ubuntu",
		OSVersion:    "22.04",
		Architecture: ArchX86_64,
		DefaultUser:  "ubuntu",
		MinDiskGB:    8,
		SourceURL:    "https://cloud-images.ubuntu.com/releases/jammy/release/ubuntu-22.04-server-cloudimg-amd64.img",
		ChecksumURL:  "https://cloud-images.ubuntu.com/releases/jammy/release/SHA256SUMS",
	},
	{
		ID:           "debian-12",
		Name:         "Debian 12 (bookworm)",
		Description:  "Debian's current stable release, generic cloud image",
		OSFamily:     "debian",
		OSVersion:    "12",
		Architecture: ArchX86_64,
		DefaultUser:  "debian",
		MinDiskGB:    8,
		SourceURL:    "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2",
	},
	{
		ID:           "rockylinux-9",
		Name:         "Rocky Linux 9",
		Description:  "RHEL compatible enterprise distribution, generic cloud image",
		OSFamily:     "rocky",
		OSVersion:    "9",
		Architecture: ArchX86_64,
		DefaultUser:  "rocky",
		MinDiskGB:    10,
		SourceURL:    "https://download.rockylinux.org/pub/rocky/9/images/x86_64/Rocky-9-GenericCloud.latest.x86_64.qcow2",
	},
	{
		ID:           "almalinux-9",
		Name:         "AlmaLinux 9",
		Description:  "RHEL compatible enterprise distribution, generic cloud image",
		OSFamily:     "almalinux",
		OSVersion:    "9",
		Architecture: ArchX86_64,
		DefaultUser:  "almalinux",
		MinDiskGB:    10,
		SourceURL:    "https://repo.almalinux.org/almalinux/9/cloud/x86_64/images/AlmaLinux-9-GenericCloud-latest.x86_64.qcow2",
	},
	{
		ID:           "fedora-44",
		Name:         "Fedora 44",
		Description:  "Fedora's current release, cloud image",
		OSFamily:     "fedora",
		OSVersion:    "44",
		Architecture: ArchX86_64,
		DefaultUser:  "fedora",
		MinDiskGB:    8,
		SourceURL:    "https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud/x86_64/images/Fedora-Cloud-Base-Generic-44-1.7.x86_64.qcow2",
		ChecksumURL:  "https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud/x86_64/images/Fedora-Cloud-44-1.7-x86_64-CHECKSUM",
	},
	// Alpine publishes no "latest" alias for its cloud images and keeps only the
	// newest patch release in the directory, so this URL names 3.22.6 and has to
	// be bumped when Alpine cuts 3.22.7. The "bios-cloudinit" variant is the one
	// that fits: Kumo leaves <os firmware> empty, so guests boot on SeaBIOS, and
	// the "tiny" variants ship without cloud-init and would ignore the NoCloud
	// seed ISO that carries the key pair and user data.
	{
		ID:           "alpine-322",
		Name:         "Alpine Linux 3.22",
		Description:  "Minimal musl distribution, needs a larger root disk for packages",
		OSFamily:     "alpine",
		OSVersion:    "3.22",
		Architecture: ArchX86_64,
		DefaultUser:  "alpine",
		MinDiskGB:    8,
		SourceURL:    "https://dl-cdn.alpinelinux.org/alpine/v3.22/releases/cloud/alpine-3.22.6-x86_64-bios-cloudinit-r0.qcow2",
	},
}
