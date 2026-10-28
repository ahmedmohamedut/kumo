// Package image owns machine images: importing them from a URL, listing them and
// deleting them. The import is a long operation, so POST /images only records
// the request and a task; the download, the checksum, the format check and the
// volume creation happen in the worker
// (docs/design/04-libvirt-integration.md §2).
package image

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// State is the lifecycle of an image. The values mirror the CHECK constraint on
// images.state (docs/design/02-data-model.md) and the ImageState enum of
// api/openapi.yaml.
type State string

const (
	// StatePending means the request is recorded and queued.
	StatePending State = "pending"
	// StateDownloading means a worker is fetching the bytes.
	StateDownloading State = "downloading"
	// StateAvailable means the volume exists and can be launched from.
	StateAvailable State = "available"
	// StateFailed means the import gave up; StateReason says why.
	StateFailed State = "failed"
	// StateDeleting means the volume is being removed.
	StateDeleting State = "deleting"
)

// Architecture is the CPU architecture of an image.
type Architecture string

// The supported architectures, per the images.architecture CHECK constraint.
const (
	ArchX86_64  Architecture = "x86_64"
	ArchAarch64 Architecture = "aarch64"
)

// Kind separates the images a user launches from the golden images the
// databases module boots. The values mirror the images.kind CHECK constraint
// (docs/design/07-databases.md section 3).
type Kind string

const (
	// KindUser is an ordinary image, offered on the Images pages.
	KindUser Kind = "user"
	// KindEngine is a golden image with a database engine already installed. It
	// is hidden from the Images pages and is what a DB instance boots from.
	KindEngine Kind = "engine"
)

// Engine is the database engine an engine image carries. It is a value rather
// than something inferred from a name, so a second engine is data plus an
// image rather than a code change.
type Engine string

// EnginePostgres is the only engine of this phase (07-databases.md R1).
const EnginePostgres Engine = "postgres"

// ChecksumAlgorithm is a hash the import verifies the download against.
type ChecksumAlgorithm string

// The supported checksum algorithms.
const (
	ChecksumSHA256 ChecksumAlgorithm = "sha256"
	ChecksumSHA512 ChecksumAlgorithm = "sha512"
)

// Checksum is the expected hash of an image. Both fields are required together.
type Checksum struct {
	Algorithm ChecksumAlgorithm `json:"algorithm"`
	Value     string            `json:"value"`
}

// IsZero reports whether no checksum was requested.
func (c Checksum) IsZero() bool { return c.Algorithm == "" && c.Value == "" }

// Image is the safe projection of an image row. Tags are always present.
type Image struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	State       State  `json:"state"`
	// StateReason explains a failure or a wait. It is null while healthy.
	StateReason *string `json:"state_reason"`
	// Progress is 0-100 while the download runs.
	Progress int `json:"progress"`
	// Format is empty until the image is available.
	Format       imageFormat  `json:"format"`
	Architecture Architecture `json:"architecture"`
	OSFamily     string       `json:"os_family"`
	OSVersion    string       `json:"os_version"`
	DefaultUser  string       `json:"default_user"`
	SourceURL    *string      `json:"source_url"`
	Checksum     *Checksum    `json:"checksum"`
	// SizeBytes is the size on disk, null until known.
	SizeBytes *int64 `json:"size_bytes"`
	// MinDiskGB is the smallest root disk the image boots from.
	MinDiskGB int `json:"min_disk_gb"`
	// Kind is "user" for an ordinary image and "engine" for a golden image the
	// databases module boots.
	Kind Kind `json:"kind"`
	// Engine and EngineVersion are set on an engine image and null on any
	// other, which is what makes "Postgres 18" resolvable to a volume.
	Engine        *Engine           `json:"engine"`
	EngineVersion *string           `json:"engine_version"`
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	Tags          map[string]string `json:"tags"`
}

// imageFormat mirrors hypervisor.ImageFormat in the JSON shape the API promises.
type imageFormat string

// Page is one page of an image listing.
type Page struct {
	Items         []Image `json:"items"`
	NextPageToken *string `json:"next_page_token"`
}

// ListInput selects a page of images.
type ListInput struct {
	// Query matches a substring of the name.
	Query string
	// State restricts the page to one state.
	State State
	// Kind restricts the page to one kind. List replaces the empty value with
	// KindUser, so a caller that forgets it cannot publish engine images on a
	// page the console reaches; an internal caller that wants every kind asks
	// for one explicitly.
	Kind Kind
	Sort SortOrder
	// PageSize and PageToken are the keyset pagination controls.
	PageSize  int
	PageToken string
}

// ImportInput is a POST /images/import body with the calling user attached.
type ImportInput struct {
	Name         string
	Description  string
	SourceURL    string
	Checksum     Checksum
	OSFamily     string
	OSVersion    string
	Architecture Architecture
	DefaultUser  string
	MinDiskGB    int
	Tags         map[string]string
	CreatedBy    string
	// Kind defaults to KindUser. KindEngine requires Engine and EngineVersion
	// and is what makes the image invisible to the Images pages.
	Kind Kind
	// Engine and EngineVersion are required with KindEngine and refused
	// without it: an ordinary image that claims an engine is a client bug, and
	// dropping the fields silently would hide it.
	Engine        Engine
	EngineVersion string
}

// UpdateInput is a PATCH /images/{id} body. A nil field is left alone; tags are
// replaced wholesale, so an empty map removes them.
type UpdateInput struct {
	Name        *string
	Description *string
	Tags        map[string]string
}

// CatalogImage is a well-known cloud image the console can offer with one click.
type CatalogImage struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Description  string       `json:"description"`
	OSFamily     string       `json:"os_family"`
	OSVersion    string       `json:"os_version"`
	Architecture Architecture `json:"architecture"`
	DefaultUser  string       `json:"default_user"`
	MinDiskGB    int          `json:"min_disk_gb"`
	SourceURL    string       `json:"source_url"`
	// ChecksumURL publishes the expected hash; it is not fetched by Kumo.
	ChecksumURL string `json:"checksum_url,omitempty"`
}

// Imported is the answer to POST /images/import: the image in its pending state
// plus the task that will advance it.
type Imported struct {
	Image
	TaskID string `json:"task_id"`
}

// Deleted is the answer to DELETE /images/{id}: the image in its deleting state
// plus the task that will remove the volume.
type Deleted struct {
	Image
	TaskID string `json:"task_id"`
}

// Options configures the service.
type Options struct {
	// Logger records every state change of an image.
	Logger *slog.Logger
	// Store is the metadata database.
	Store *store.Store
	// Hypervisor registers and removes image volumes.
	Hypervisor hypervisor.Hypervisor
	// Tasks creates the import and delete tasks.
	Tasks *task.Service
	// Downloader fetches the image bytes under the SSRF policy.
	Downloader *imagefetch.Downloader
	// ImagesPool is the libvirt pool that holds image volumes.
	ImagesPool string
	// TempDir is where a download is staged before it becomes a volume. It must
	// be on the same filesystem as the pool for the rename to be cheap.
	TempDir string
	// MaxBytes refuses a download larger than this. Zero means no limit.
	MaxBytes int64
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Sentinel errors of the constructor, so a wiring mistake reads differently
// from a runtime failure.
var (
	// ErrNoLogger means Options.Logger was nil.
	ErrNoLogger = errors.New("image: a logger is required")
	// ErrNoStore means Options.Store was nil.
	ErrNoStore = errors.New("image: a store is required")
	// ErrNoHypervisor means Options.Hypervisor was nil.
	ErrNoHypervisor = errors.New("image: a hypervisor is required")
	// ErrNoTasks means Options.Tasks was nil.
	ErrNoTasks = errors.New("image: a task service is required")
	// ErrNoDownloader means Options.Downloader was nil.
	ErrNoDownloader = errors.New("image: a downloader is required")
	// ErrNoPool means Options.ImagesPool was empty.
	ErrNoPool = errors.New("image: an images pool name is required")
	// ErrNoTempDir means Options.TempDir was empty.
	ErrNoTempDir = errors.New("image: a temporary directory is required")
	// ErrNoMaxBytes means neither the service nor the downloader had a size
	// limit, so a download could fill the disk.
	ErrNoMaxBytes = errors.New("image: a download size limit is required")
)

// Service owns images.
type Service struct {
	logger     *slog.Logger
	store      *store.Store
	hv         hypervisor.Hypervisor
	tasks      *task.Service
	downloader *imagefetch.Downloader
	imagesPool string
	tempDir    string
	maxBytes   int64
	now        func() time.Time
}

// New builds the service.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Logger == nil:
		return nil, ErrNoLogger
	case opts.Store == nil:
		return nil, ErrNoStore
	case opts.Hypervisor == nil:
		return nil, ErrNoHypervisor
	case opts.Tasks == nil:
		return nil, ErrNoTasks
	case opts.Downloader == nil:
		return nil, ErrNoDownloader
	case opts.ImagesPool == "":
		return nil, ErrNoPool
	case opts.TempDir == "":
		return nil, ErrNoTempDir
	}

	if opts.Now == nil {
		opts.Now = time.Now
	}

	// A limit has to exist somewhere, or a hostile or broken server can stream
	// until the disk is full. Whichever side has one wins, so a caller that only
	// configures the downloader is still bounded.
	maxBytes := max(opts.MaxBytes, opts.Downloader.MaxBytes())
	if maxBytes <= 0 {
		return nil, ErrNoMaxBytes
	}

	return &Service{
		logger:     opts.Logger,
		store:      opts.Store,
		hv:         opts.Hypervisor,
		tasks:      opts.Tasks,
		downloader: opts.Downloader,
		imagesPool: opts.ImagesPool,
		tempDir:    opts.TempDir,
		maxBytes:   maxBytes,
		now:        opts.Now,
	}, nil
}

// Import records the request and queues the download. The response is 202 with
// the image in pending and a Location pointing at the task
// (docs/design/01-api.md §Images).
func (s *Service) Import(ctx context.Context, in ImportInput) (Imported, error) {
	fields := s.validateImport(in)
	if len(fields) > 0 {
		return Imported{}, &service.ValidationError{Message: "the image is not acceptable", Fields: fields}
	}

	if in.Kind == "" {
		in.Kind = KindUser
	}
	if in.Architecture == "" {
		in.Architecture = ArchX86_64
	}
	if in.OSFamily == "" {
		in.OSFamily = "linux"
	}

	var imported Imported
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		now := formatTime(s.now())
		row, err := tx.CreateImage(ctx, sqlc.CreateImageParams{
			ID:               newImageID(),
			Name:             in.Name,
			Description:      in.Description,
			OsFamily:         in.OSFamily,
			OsVersion:        in.OSVersion,
			Architecture:     string(in.Architecture),
			DefaultUser:      in.DefaultUser,
			Format:           string(hypervisor.FormatQCOW2), // provisional; the header decides
			State:            string(StatePending),
			StateReason:      sql.NullString{},
			Progress:         0,
			SourceUrl:        sql.NullString{String: in.SourceURL, Valid: true},
			ChecksumAlgo:     nullableChecksumAlgorithm(in.Checksum),
			ChecksumValue:    nullableString(in.Checksum.Value),
			SizeBytes:        sql.NullInt64{},
			VirtualSizeBytes: sql.NullInt64{},
			MinDiskGb:        int64(in.MinDiskGB),
			VolumeName:       sql.NullString{},
			CreatedBy:        nullableString(in.CreatedBy),
			CreatedAt:        now,
			UpdatedAt:        now,
			Kind:             string(in.Kind),
			Engine:           nullableString(string(in.Engine)),
			EngineVersion:    nullableString(in.EngineVersion),
		})
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: an image named %q already exists",
					service.ErrAlreadyExists, in.Name)
			}
			return fmt.Errorf("create the image row: %w", err)
		}

		if err := replaceTags(ctx, tx, row.ID, in.Tags); err != nil {
			return err
		}

		created, err := s.tasks.CreateInTx(ctx, tx, task.TypeImageImport, row.ID,
			importPayload{ImageID: row.ID})
		if err != nil {
			return err
		}

		imported = Imported{
			Image:  imageFromRow(row, in.Tags),
			TaskID: created.ID,
		}
		return nil
	})
	if err != nil {
		return Imported{}, err
	}

	s.logger.InfoContext(ctx, "image: import queued",
		"image_id", imported.ID, "name", imported.Name, "task_id", imported.TaskID)

	return imported, nil
}

// importPayload is the task input of an image import. It only names the image:
// the URL, the checksum and every other input live on the row, so SQLite stays
// the single source of truth and a retry cannot act on stale task data.
type importPayload struct {
	ImageID string `json:"image_id"`
}

// deletePayload is the task input of an image delete.
type deletePayload struct {
	ImageID string `json:"image_id"`
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nullableString(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func nullableChecksumAlgorithm(checksum Checksum) sql.NullString {
	if checksum.Algorithm == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: string(checksum.Algorithm), Valid: true}
}
