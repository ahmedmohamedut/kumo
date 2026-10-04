package instance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/capacity"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/keypair"
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

	var launched Instance
	err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		now := formatTime(s.now())
		row, err := tx.CreateInstance(ctx, sqlc.CreateInstanceParams{
			ID:           newInstanceID(),
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
			CreatedBy:    nullableString(in.CreatedBy),
			LaunchedAt:   now,
			UpdatedAt:    now,
		})
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: an instance named %q already exists",
					service.ErrAlreadyExists, in.Name)
			}
			return fmt.Errorf("create the instance row: %w", err)
		}

		if err := replaceTags(ctx, tx, row.ID, in.Tags); err != nil {
			return err
		}

		created, err := s.tasks.CreateInTx(ctx, tx, task.TypeInstanceLaunch, row.ID,
			launchPayload{InstanceID: row.ID})
		if err != nil {
			return err
		}

		launched = instanceFromRow(row, imageName{ID: source.ID, Name: source.Name}, in.Tags)
		launched.TaskID = &created.ID
		return nil
	})
	if err != nil {
		return Launched{}, err
	}

	s.logger.InfoContext(ctx, "instance: launch queued",
		"instance_id", launched.ID, "name", launched.Name,
		"image_id", launched.ImageID, "instance_type", launched.InstanceType, "task_id", deref(launched.TaskID))

	return launched, nil
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

// deref renders a nullable string for a log line.
func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
