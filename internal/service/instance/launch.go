package instance

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/network"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/instancetype"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// LaunchInput is a POST /instances body with the calling user attached.
type LaunchInput struct {
	Name         string
	ImageID      string
	InstanceType string
	KeyPairID    string
	// SubnetID attaches the instance to one subnet. Empty attaches it to the
	// default subnet of the default VPC.
	SubnetID string
	// RootDiskGB defaults to the image's min_disk_gb.
	RootDiskGB int
	UserData   string
	// Count is the number of instances the request asks for.
	Count int
	Tags  map[string]string
	// CreatedBy is the user the launch is attributed to.
	CreatedBy string
}

// Launched is the answer to POST /instances: the instance in its pending state,
// with task_id naming the launch task. It carries no extra field, because
// api/openapi.yaml answers this endpoint with an Instance and the task is part of
// it.
type Launched = Instance

// Launch records the request and queues the work. The response is 202 with the
// instance in pending; nothing here touches libvirt, because a root disk, a
// domain and a DHCP lease are all slower than an HTTP request
// (docs/design/04-libvirt-integration.md §3).
func (s *Service) Launch(ctx context.Context, in LaunchInput) (Launched, error) {
	fields := validateLaunch(in)
	if len(fields) > 0 {
		return Launched{}, &service.ValidationError{Message: "the launch request is not acceptable", Fields: fields}
	}

	preset, err := s.types.Get(ctx, in.InstanceType)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return Launched{}, &service.ValidationError{
				Message: "the launch request is not acceptable",
				Fields: []service.FieldError{{
					Field:  "instance_type",
					Reason: fmt.Sprintf("no instance type %q exists", in.InstanceType),
				}},
			}
		}
		return Launched{}, fmt.Errorf("read the instance type %q: %w", in.InstanceType, err)
	}

	source, err := s.images.LaunchSource(ctx, in.ImageID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			return Launched{}, &service.ValidationError{
				Message: "the launch request is not acceptable",
				Fields:  []service.FieldError{{Field: "image_id", Reason: "no such image"}},
			}
		}
		return Launched{}, fmt.Errorf("read the image %q: %w", in.ImageID, err)
	}
	if source.State != image.StateAvailable {
		return Launched{}, &service.ValidationError{
			Message: "the launch request is not acceptable",
			Fields: []service.FieldError{{
				Field:  "image_id",
				Reason: fmt.Sprintf("the image is %s; only an available image can be launched", source.State),
			}},
		}
	}

	if in.RootDiskGB == 0 {
		in.RootDiskGB = source.MinDiskGB
	}
	if fields := validateRootDisk(in.RootDiskGB, source.MinDiskGB); len(fields) > 0 {
		return Launched{}, &service.ValidationError{Message: "the launch request is not acceptable", Fields: fields}
	}

	// The key pair is looked up here rather than in the worker so a bad id is a
	// 400 on the request instead of a failed launch a minute later.
	var keyPair keypair.Detail
	if in.KeyPairID != "" {
		keyPair, err = s.keyPairs.Get(ctx, in.KeyPairID)
		if err != nil {
			if errors.Is(err, service.ErrNotFound) {
				return Launched{}, &service.ValidationError{
					Message: "the launch request is not acceptable",
					Fields:  []service.FieldError{{Field: "key_pair_id", Reason: "no such key pair"}},
				}
			}
			return Launched{}, fmt.Errorf("read the key pair %q: %w", in.KeyPairID, err)
		}
	}

	if err := s.checkCapacity(ctx, capacity.Build(preset.VCPUs, int64(preset.MemoryMiB), int64(in.RootDiskGB))); err != nil {
		return Launched{}, err
	}

	// The subnet the instance attaches to is resolved before the lock: an
	// explicit id that does not exist is a 400, and an omitted one names the
	// default subnet (which missing is a conflict the operator must build away).
	allocSubnet, err := s.resolveLaunchSubnet(ctx, in.SubnetID)
	if err != nil {
		return Launched{}, err
	}

	// A launch allocates out of the subnet inside the transaction below while a
	// subnet delete would remove the row; both take the very same key the
	// subnet service deletes under, so a launch that races a delete refuses
	// instead of allocating out of a range that vanishes underneath it
	// (docs/design/01-api.md §Subnets).
	release, err := s.subnetLocks.TryAcquire(subnet.LockKey(allocSubnet.ID), "launch")
	if err != nil {
		holder, held := s.subnetLocks.Holder(subnet.LockKey(allocSubnet.ID))
		if !held {
			// Unreachable in practice: a held lock is what made TryAcquire fail.
			holder = "another operation"
		}
		return Launched{}, fmt.Errorf("%w: the subnet %q is busy with %s",
			service.ErrConflict, allocSubnet.ID, holder)
	}
	defer release()

	var launched Instance
	err = s.launchInTx(ctx, in, preset, source, keyPair, allocSubnet, &launched)
	if err != nil {
		return Launched{}, err
	}

	s.logger.InfoContext(ctx, "instance: launch queued",
		"instance_id", launched.ID, "name", launched.Name,
		"image_id", launched.ImageID, "instance_type", launched.InstanceType,
		"subnet_id", launched.SubnetID, "task_id", deref(launched.TaskID))

	return launched, nil
}

// launchInTx writes the instance row, its interface and its launch task, retrying
// the whole transaction when the address allocation collides with another
// launch. The collision only shows up at write time, because the read of the
// allocated addresses and the insert that claims one are two statements; the
// unique index on (subnet_id, private_ip) is what turns the race into an error,
// and a fresh allocation is what resolves it.
func (s *Service) launchInTx(ctx context.Context, in LaunchInput, preset instancetype.Type,
	source image.LaunchSource, keyPair keypair.Detail, allocSubnet sqlc.Subnet, launched *Instance) error {
	var err error
	for attempt := 0; attempt < maxLaunchAllocationAttempts; attempt++ {
		err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
			return s.launchAllocate(ctx, tx, in, preset, source, keyPair, allocSubnet, launched)
		})
		if err == nil {
			return nil
		}
		// A collision is retryable, and so is a busy database: both raced
		// another launch and both resolve by trying again.
		if !errors.Is(err, errMACConflict) && !isDBLocked(err) {
			return err
		}
	}
	return err
}

// launchAllocate performs one launch transaction: it allocates an address and a
// MAC, writes the instance row, its interface and the launch task, and projects
// the answer. The rows and the allocation live in one transaction so a launch
// that fails half way leaves nothing allocated behind it.
func (s *Service) launchAllocate(ctx context.Context, tx *store.Tx, in LaunchInput, preset instancetype.Type,
	source image.LaunchSource, keyPair keypair.Detail, allocSubnet sqlc.Subnet, launched *Instance) error {
	now := formatTime(s.now())

	// The gateway is the one address of the VPC that is never for sale; the
	// subnet row only carries its own CIDR.
	vpcRow, err := tx.GetVPC(ctx, sqlc.GetVPCParams{ID: allocSubnet.VpcID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: the VPC %q of the subnet %q is gone",
				service.ErrConflict, allocSubnet.VpcID, allocSubnet.ID)
		}
		return fmt.Errorf("read the VPC %q: %w", allocSubnet.VpcID, err)
	}
	gateway, err := netip.ParseAddr(vpcRow.GatewayIp)
	if err != nil {
		return fmt.Errorf("parse the gateway %q of VPC %q: %w", vpcRow.GatewayIp, vpcRow.ID, err)
	}

	allocated, err := tx.ListAllocatedIPsInSubnet(ctx, sqlc.ListAllocatedIPsInSubnetParams{SubnetID: allocSubnet.ID})
	if err != nil {
		return fmt.Errorf("list the allocated addresses of subnet %q: %w", allocSubnet.ID, err)
	}
	ip, err := allocateIP(allocSubnet.Cidr, gateway, allocated)
	if err != nil {
		return err
	}
	mac := randomMAC()

	instanceID := newInstanceID()
	row, err := tx.CreateInstance(ctx, sqlc.CreateInstanceParams{
		ID:           instanceID,
		Name:         in.Name,
		State:        string(StatePending),
		StateReason:  sql.NullString{},
		ImageID:      source.ID,
		InstanceType: preset.ID,
		Vcpus:        int64(preset.VCPUs),
		MemoryMib:    int64(preset.MemoryMiB),
		RootDiskGb:   int64(in.RootDiskGB),
		KeyPairID:    nullableString(keyPair.ID),
		KeyPairName:  nullableString(keyPair.Name),
		UserData:     nullableString(in.UserData),
		MacAddress:   nullableString(mac),
		PrivateIp:    nullableString(ip.String()),
		CreatedBy:    nullableString(in.CreatedBy),
		LaunchedAt:   now,
		UpdatedAt:    now,
	})
	if err != nil {
		if isUniqueViolation(err) {
			// With an interface the rows are inserted in one transaction, so the
			// only unique index an instance insert can trip is the live name.
			return fmt.Errorf("%w: an instance named %q already exists",
				service.ErrAlreadyExists, in.Name)
		}
		return fmt.Errorf("create the instance row: %w", err)
	}

	eni, err := tx.CreateNetworkInterface(ctx, sqlc.CreateNetworkInterfaceParams{
		ID:          id.NetworkInterface(),
		InstanceID:  row.ID,
		SubnetID:    allocSubnet.ID,
		DeviceIndex: 0,
		MacAddress:  mac,
		PrivateIp:   ip.String(),
		CreatedAt:   now,
	})
	if err != nil {
		// A unique violation naming network_interfaces is an address or MAC that
		// another launch claimed between the read and this insert.
		if isUniqueViolation(err) && strings.Contains(err.Error(), "network_interfaces") {
			return fmt.Errorf("allocate the interface of %q: %w", row.ID, errMACConflict)
		}
		return fmt.Errorf("create the network interface: %w", err)
	}

	if err := replaceTags(ctx, tx, row.ID, in.Tags); err != nil {
		return err
	}

	created, err := s.tasks.CreateInTx(ctx, tx, task.TypeInstanceLaunch, row.ID,
		launchPayload{InstanceID: row.ID})
	if err != nil {
		return err
	}

	out := instanceFromRow(row, imageName{ID: source.ID, Name: source.Name}, in.Tags)
	out.NetworkInterfaces = []Interface{{
		ID:          eni.ID,
		DeviceIndex: int(eni.DeviceIndex),
		MACAddress:  eni.MacAddress,
		PrivateIP:   eni.PrivateIp,
		SubnetID:    eni.SubnetID,
		VPCID:       vpcRow.ID,
	}}
	out.VPCID = &vpcRow.ID
	out.SubnetID = &allocSubnet.ID
	out.TaskID = &created.ID
	*launched = out
	return nil
}

// maxLaunchAllocationAttempts bounds the retries of a collision: each retry
// reads the addresses again and mints a fresh MAC, so a pick that loses a race
// once loses it for a different pick the second time. Five attempts cover a
// busy moment without stalling the request.
const maxLaunchAllocationAttempts = 5

// errMACConflict is the retryable allocation collision of launchAllocate: the
// interface's MAC or address was claimed by another launch between the read and
// the insert. It is a sentinel because the retry is the concern of launchInTx,
// not of the api layer.
var errMACConflict = errors.New("instance: the interface allocation collided with another launch")

// resolveLaunchSubnet names the subnet an instance attaches to. An explicit
// subnet must exist, or the request is a 400 naming the field; an omitted one
// falls back to the default subnet of the default VPC, whose absence is a
// conflict: the operator has to create a VPC before instances can exist.
func (s *Service) resolveLaunchSubnet(ctx context.Context, subnetID string) (sqlc.Subnet, error) {
	if subnetID == "" {
		subnetRow, err := s.store.GetDefaultSubnet(ctx)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return sqlc.Subnet{}, fmt.Errorf(
					"%w: no default subnet; create a VPC first or pass subnet_id", service.ErrConflict)
			}
			return sqlc.Subnet{}, fmt.Errorf("read the default subnet: %w", err)
		}
		return subnetRow, nil
	}

	subnetRow, err := s.store.GetSubnet(ctx, sqlc.GetSubnetParams{ID: subnetID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlc.Subnet{}, &service.ValidationError{
				Message: "the launch request is not acceptable",
				Fields:  []service.FieldError{{Field: "subnet_id", Reason: "no such subnet"}},
			}
		}
		return sqlc.Subnet{}, fmt.Errorf("read the subnet %q: %w", subnetID, err)
	}
	return subnetRow, nil
}

// allocateIP chooses the lowest free address of a subnet, given the addresses
// already handed out. It refuses a subnet whose every usable address is taken.
func allocateIP(subnetCIDR string, gateway netip.Addr, allocated []string) (netip.Addr, error) {
	prefix, err := netip.ParsePrefix(subnetCIDR)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse the subnet CIDR %q: %w", subnetCIDR, err)
	}

	taken := make(map[netip.Addr]struct{}, len(allocated))
	for _, address := range allocated {
		if addr, err := netip.ParseAddr(address); err == nil {
			taken[addr] = struct{}{}
		}
	}

	var free netip.Addr
	network.ForEachUsable(prefix, gateway, func(addr netip.Addr) bool {
		if _, ok := taken[addr]; ok {
			return true
		}
		free = addr
		return false
	})
	if !free.IsValid() {
		return netip.Addr{}, fmt.Errorf("%w: the subnet %s has no free address",
			service.ErrQuota, prefix)
	}
	return free, nil
}

// randomMAC mints a locally administered unicast MAC in QEMU's 52:54:00 space,
// the same prefix libvirt would pick itself. The last three bytes are random: a
// hand-picked value could collide with a MAC the interface table already holds,
// and the unique index would refuse the launch.
func randomMAC() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("52:54:00:%02x:%02x:%02x", b[0], b[1], b[2])
}

// launchPayload is the task input of a launch. It only names the instance: the
// image, the key pair, the user data and every other input live on the row, so
// SQLite stays the single source of truth and a retry cannot act on stale task
// data.
type launchPayload struct {
	InstanceID string `json:"instance_id"`
}

// checkCapacity refuses a launch the node cannot hold. The arithmetic is the one
// GET /system/capacity reports, so the console sees the same numbers the refusal
// is based on.
func (s *Service) checkCapacity(ctx context.Context, want capacity.Request) error {
	report, err := s.capacity.Capacity(ctx)
	if err != nil {
		return fmt.Errorf("measure the capacity of this node: %w", err)
	}

	free := capacity.Free(report, s.limits)
	if err := capacity.Check(free, want, s.limits); err != nil {
		return err
	}
	return nil
}

// validateLaunch checks the request itself, before anything is read. It cannot
// judge root_disk_gb against the image, because the image is not loaded yet; that
// half is validateRootDisk.
func validateLaunch(in LaunchInput) []service.FieldError {
	var fields []service.FieldError

	if reason := validateName(in.Name); reason != "" {
		fields = append(fields, service.FieldError{Field: "name", Reason: reason})
	}
	if strings.TrimSpace(in.ImageID) == "" {
		fields = append(fields, service.FieldError{Field: "image_id", Reason: "is required"})
	}
	if strings.TrimSpace(in.InstanceType) == "" {
		fields = append(fields, service.FieldError{Field: "instance_type", Reason: "is required"})
	}
	if in.SubnetID != "" && !id.HasResourceShape(id.PrefixSubnet, in.SubnetID) {
		fields = append(fields, service.FieldError{
			Field:  "subnet_id",
			Reason: "must be a subnet-… id",
		})
	}
	if in.RootDiskGB < 0 || in.RootDiskGB > MaxRootDiskGB {
		fields = append(fields, service.FieldError{
			Field:  "root_disk_gb",
			Reason: fmt.Sprintf("must be at most %d GiB", MaxRootDiskGB),
		})
	}
	if len(in.UserData) > MaxUserDataBytes {
		fields = append(fields, service.FieldError{
			Field:  "user_data",
			Reason: fmt.Sprintf("must be at most %d KiB", MaxUserDataBytes/1024),
		})
	}
	// The contract allows count 1-10, but this milestone launches one instance
	// per request. Naming several would mean inventing a numbering scheme for
	// them (i-2-web, web-2?) that no design document specifies, so an explicit
	// refusal is better than a convention the API did not ask for.
	if in.Count != 0 && in.Count != 1 {
		fields = append(fields, service.FieldError{
			Field:  "count",
			Reason: fmt.Sprintf("must be 1 in this release (the contract allows up to %d)", MaxCount),
		})
	}
	fields = append(fields, validateTags(in.Tags)...)

	return fields
}

// validateRootDisk judges the requested size against the image, which is the half
// of the size rule that needs the image row.
func validateRootDisk(gb, imageMinGB int) []service.FieldError {
	if gb > MaxRootDiskGB {
		return []service.FieldError{{
			Field:  "root_disk_gb",
			Reason: fmt.Sprintf("must be at most %d GiB", MaxRootDiskGB),
		}}
	}
	// The overlay cannot be smaller than the image it is a delta of: the guest
	// would see a disk that ends before its own filesystem.
	if imageMinGB > 0 && gb < imageMinGB {
		return []service.FieldError{{
			Field:  "root_disk_gb",
			Reason: fmt.Sprintf("the image needs at least %d GiB", imageMinGB),
		}}
	}
	if gb < 1 {
		return []service.FieldError{{Field: "root_disk_gb", Reason: "must be at least 1 GiB"}}
	}
	return nil
}

// validateName checks an instance name against the RFC 1123 label the schema and
// the console both assume: lowercase, no leading or trailing dash.
func validateName(name string) string {
	switch {
	case strings.TrimSpace(name) == "":
		return "is required"
	case len(name) > MaxNameLength:
		return fmt.Sprintf("must be at most %d characters", MaxNameLength)
	case !isLabel(name):
		return "must be lowercase letters, digits and dashes, and cannot start or end with a dash"
	}
	return ""
}

// isLabel reports whether name is an RFC 1123 label.
func isLabel(name string) bool {
	if name == "" || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// validateTags bounds the tag map the way every other resource does.
func validateTags(tags map[string]string) []service.FieldError {
	var fields []service.FieldError
	for key, value := range tags {
		if strings.TrimSpace(key) == "" {
			fields = append(fields, service.FieldError{Field: "tags", Reason: "a tag key must not be empty"})
			continue
		}
		if len(key) > 128 || len(value) > 256 {
			fields = append(fields, service.FieldError{
				Field:  "tags",
				Reason: "a tag key is at most 128 characters and a value at most 256",
			})
		}
	}
	return fields
}

// replaceTags writes the tag set of an instance.
func replaceTags(ctx context.Context, tx *store.Tx, instanceID string, tags map[string]string) error {
	if err := tx.DeleteResourceTags(ctx, sqlc.DeleteResourceTagsParams{ResourceID: instanceID}); err != nil {
		return fmt.Errorf("delete the tags of %q: %w", instanceID, err)
	}
	for key, value := range tags {
		if err := tx.SetTag(ctx, sqlc.SetTagParams{
			ResourceID: instanceID, Key: key, Value: value,
		}); err != nil {
			return fmt.Errorf("set the tag %q of %q: %w", key, instanceID, err)
		}
	}
	return nil
}

// newInstanceID mints the id an instance is launched under.
func newInstanceID() string { return id.Resource(id.PrefixInstance) }

// consoleLogPath is where the serial log of an instance is written. It lives
// beside the data dir rather than in a pool, because it is not a volume: nothing
// deletes it when the instance is terminated, and the console log of an
// instance that failed to launch is worth reading after that.
func (s *Service) consoleLogPath(instanceID string) string {
	return filepath.Join(s.consoleDir, instanceID+".log")
}

// ensureConsoleDir creates the console directory. libvirt cannot write the
// serial log into a directory that does not exist, and the failure would only
// show up as a domain that boots with no console.
func (s *Service) ensureConsoleDir() error {
	return ensureDir(s.consoleDir, 0o755)
}

// ensureConsoleLog creates the serial log before the domain starts. libvirt's
// virtlogd would create it itself, but as root with mode 0600, and the service
// runs unprivileged: it could never read the file to serve the console-output
// endpoint. A file that exists is only appended to by virtlogd, so its owner and
// mode stay the ones set here. The chmod is explicit because the service's umask
// is stricter than 0644.
func (s *Service) ensureConsoleLog(instanceID string) error {
	path := s.consoleLogPath(instanceID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("create the console log %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close the console log %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		return fmt.Errorf("chmod the console log %s: %w", path, err)
	}
	return nil
}

// readConsoleLog returns the last maxBytes of an instance's console log file
// straight from disk. It is the answer for an instance with no domain: a launch
// that failed has been rolled back, but the guest's last words are still here.
func (s *Service) readConsoleLog(instanceID string, maxBytes int) ([]byte, error) {
	file, err := os.Open(s.consoleLogPath(instanceID))
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // a read-only handle has nothing to flush

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size > int64(maxBytes) {
		if _, err := file.Seek(-int64(maxBytes), io.SeekEnd); err != nil {
			return nil, err
		}
		size = int64(maxBytes)
	}
	buf := make([]byte, size)
	n, err := io.ReadFull(file, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return buf[:n], nil
}

// deref renders a nullable string for a log line.
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
