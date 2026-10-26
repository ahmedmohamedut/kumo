// Package accesskey owns the object storage access keys: S3 credentials an
// administrator hands to an application, each limited to chosen buckets,
// read-only or read-write (docs/design/06-object-storage.md §5).
//
// A key is a RustFS user with a policy of its own; the access_keys table is
// what Kumo knows about it. The secret key is generated here, sent to RustFS,
// returned once in the create answer, and never stored or logged, the same
// rule as a generated private key.
package accesskey

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Limits of 01-api.md §Object storage.
const (
	MaxNameLength = 64
	MaxBuckets    = 100
	// accessKeyPrefix marks a key Kumo issued, the way AKIA marks an AWS one.
	accessKeyPrefix = "KUMO"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Sentinel errors of the constructor.
var (
	ErrNoLogger      = errors.New("accesskey: a logger is required")
	ErrNoStore       = errors.New("accesskey: a store is required")
	ErrNoObjectStore = errors.New("accesskey: an object store is required")
)

// State is whether the object store still has the key.
type State string

// The key states.
const (
	StateActive State = "active"
	// StateMissing means the RustFS user is gone (removed with another tool):
	// the key no longer works, and deleting it here tidies the row.
	StateMissing State = "missing"
)

// AccessKey is one key as the API returns it, without its secret.
type AccessKey struct {
	AccessKey string                  `json:"access_key"`
	Name      string                  `json:"name"`
	Level     objectstore.AccessLevel `json:"level"`
	Buckets   []string                `json:"buckets"`
	State     State                   `json:"state"`
	CreatedBy *string                 `json:"created_by"`
	CreatedAt time.Time               `json:"created_at"`
	UpdatedAt time.Time               `json:"updated_at"`
}

// Created is the answer to a create: the key and, this once, its secret.
type Created struct {
	AccessKey
	SecretKey string `json:"secret_key"`
}

// CreateInput is a POST /s3/access-keys body.
type CreateInput struct {
	Name      string
	Level     objectstore.AccessLevel
	Buckets   []string
	CreatedBy string
}

// UpdateInput is a PATCH /s3/access-keys/{id} body; nil fields are left alone.
type UpdateInput struct {
	Name    *string
	Level   *objectstore.AccessLevel
	Buckets *[]string
}

// Options configures the service.
type Options struct {
	Logger  *slog.Logger
	Store   *store.Store
	Objects objectstore.ObjectStore
}

// Service owns the access keys.
type Service struct {
	logger  *slog.Logger
	store   *store.Store
	objects objectstore.ObjectStore
	now     func() time.Time
}

// New builds the service.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Logger == nil:
		return nil, ErrNoLogger
	case opts.Store == nil:
		return nil, ErrNoStore
	case opts.Objects == nil:
		return nil, ErrNoObjectStore
	}
	return &Service{logger: opts.Logger, store: opts.Store, objects: opts.Objects, now: time.Now}, nil
}

// List returns every key Kumo issued, by name, with whether RustFS still has
// it. Users made on RustFS with other tools are not Kumo's keys and are not
// listed.
func (s *Service) List(ctx context.Context) ([]AccessKey, error) {
	present, err := s.present(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.store.ListAccessKeyRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("accesskey: list the rows: %w", err)
	}
	grants, err := s.store.ListAccessKeyBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("accesskey: list the grants: %w", err)
	}
	buckets := map[string][]string{}
	for _, g := range grants {
		buckets[g.AccessKey] = append(buckets[g.AccessKey], g.Bucket)
	}

	keys := make([]AccessKey, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, fromRow(sqlc.GetAccessKeyRowRow(row), buckets[row.AccessKey], present[row.AccessKey]))
	}
	return keys, nil
}

// Get returns one key.
func (s *Service) Get(ctx context.Context, accessKey string) (AccessKey, error) {
	row, err := s.store.GetAccessKeyRow(ctx, sqlc.GetAccessKeyRowParams{AccessKey: accessKey})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return AccessKey{}, fmt.Errorf("%w: no access key %q", service.ErrNotFound, accessKey)
		}
		return AccessKey{}, fmt.Errorf("accesskey: read %q: %w", accessKey, err)
	}
	buckets, err := s.store.ListBucketsOfAccessKey(ctx, sqlc.ListBucketsOfAccessKeyParams{AccessKey: accessKey})
	if err != nil {
		return AccessKey{}, fmt.Errorf("accesskey: read the grant of %q: %w", accessKey, err)
	}
	present, err := s.present(ctx)
	if err != nil {
		return AccessKey{}, err
	}
	return fromRow(row, buckets, present[accessKey]), nil
}

// Create issues a key: it generates the id and the secret, creates the user
// and its policy on RustFS, then records the key. If recording fails, the
// user is removed again, so a key that Kumo does not know never works.
func (s *Service) Create(ctx context.Context, in CreateInput) (Created, error) {
	buckets := normalise(in.Buckets)
	fields := validateName(in.Name)
	fields = append(fields, validateLevel(in.Level)...)
	fields = append(fields, s.validateBuckets(ctx, buckets)...)
	if len(fields) > 0 {
		return Created{}, &service.ValidationError{Message: "the access key is not acceptable", Fields: fields}
	}
	if err := s.nameFree(ctx, in.Name, ""); err != nil {
		return Created{}, err
	}

	accessKey, secret, err := generate()
	if err != nil {
		return Created{}, err
	}
	grant := objectstore.Grant{Buckets: buckets, Level: in.Level}
	if err := s.objects.CreateAccessKey(ctx, objectstore.AccessKeySpec{
		AccessKey: accessKey, SecretKey: secret, Grant: grant,
	}); err != nil {
		return Created{}, mapErr(err)
	}

	now := formatTime(s.now())
	err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		if err := tx.CreateAccessKeyRow(ctx, sqlc.CreateAccessKeyRowParams{
			AccessKey: accessKey,
			Name:      in.Name,
			Level:     string(in.Level),
			CreatedBy: sql.NullString{String: in.CreatedBy, Valid: in.CreatedBy != ""},
			CreatedAt: now,
		}); err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint failed") {
				return fmt.Errorf("%w: an access key named %q already exists", service.ErrAlreadyExists, in.Name)
			}
			return fmt.Errorf("accesskey: record %s: %w", accessKey, err)
		}
		return setBuckets(ctx, tx, accessKey, buckets)
	})
	if err != nil {
		if undoErr := s.objects.DeleteAccessKey(context.WithoutCancel(ctx), accessKey); undoErr != nil {
			s.logger.ErrorContext(ctx, "accesskey: a key Kumo could not record is still on the object store",
				"access_key", accessKey, "error", undoErr)
		}
		return Created{}, err
	}

	s.logger.InfoContext(ctx, "accesskey: created",
		"access_key", accessKey, "name", in.Name, "level", in.Level, "buckets", buckets, "user_id", in.CreatedBy)
	key, err := s.Get(ctx, accessKey)
	if err != nil {
		return Created{}, err
	}
	return Created{AccessKey: key, SecretKey: secret}, nil
}

// Update renames a key or changes what it reaches. A change of buckets or
// level rewrites its policy on RustFS, which applies to the key's next
// request; the secret is unchanged.
func (s *Service) Update(ctx context.Context, accessKey string, in UpdateInput) (AccessKey, error) {
	current, err := s.Get(ctx, accessKey)
	if err != nil {
		return AccessKey{}, err
	}

	next := current
	var fields []service.FieldError
	if in.Name != nil {
		next.Name = *in.Name
		fields = append(fields, validateName(next.Name)...)
	}
	if in.Level != nil {
		next.Level = *in.Level
		fields = append(fields, validateLevel(next.Level)...)
	}
	if in.Buckets != nil {
		next.Buckets = normalise(*in.Buckets)
		fields = append(fields, s.validateBuckets(ctx, next.Buckets)...)
	}
	if len(fields) > 0 {
		return AccessKey{}, &service.ValidationError{Message: "the access key update is not acceptable", Fields: fields}
	}
	if next.Name != current.Name {
		if err := s.nameFree(ctx, next.Name, accessKey); err != nil {
			return AccessKey{}, err
		}
	}

	grantChanged := next.Level != current.Level || !slices.Equal(next.Buckets, current.Buckets)
	if grantChanged {
		if current.State == StateMissing {
			return AccessKey{}, fmt.Errorf("%w: access key %s is gone from the object store; delete it and issue a new one",
				service.ErrConflict, accessKey)
		}
		if err := s.objects.SetAccessKeyPolicy(ctx, accessKey, objectstore.Grant{
			Buckets: next.Buckets, Level: next.Level,
		}); err != nil {
			return AccessKey{}, mapErr(err)
		}
	}

	err = s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		if err := tx.UpdateAccessKeyRow(ctx, sqlc.UpdateAccessKeyRowParams{
			AccessKey: accessKey, Name: next.Name, Level: string(next.Level), UpdatedAt: formatTime(s.now()),
		}); err != nil {
			return fmt.Errorf("accesskey: update %s: %w", accessKey, err)
		}
		return setBuckets(ctx, tx, accessKey, next.Buckets)
	})
	if err != nil {
		return AccessKey{}, err
	}

	s.logger.InfoContext(ctx, "accesskey: updated", "access_key", accessKey,
		"level", next.Level, "buckets", next.Buckets, "grant_changed", grantChanged)
	return s.Get(ctx, accessKey)
}

// Delete revokes a key on RustFS, then drops the row. A key already gone
// from RustFS is simply tidied away.
func (s *Service) Delete(ctx context.Context, accessKey string) error {
	if _, err := s.store.GetAccessKeyRow(ctx, sqlc.GetAccessKeyRowParams{AccessKey: accessKey}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: no access key %q", service.ErrNotFound, accessKey)
		}
		return fmt.Errorf("accesskey: read %q: %w", accessKey, err)
	}
	if err := s.objects.DeleteAccessKey(ctx, accessKey); err != nil {
		return mapErr(err)
	}
	if _, err := s.store.DeleteAccessKeyRow(ctx, sqlc.DeleteAccessKeyRowParams{AccessKey: accessKey}); err != nil {
		return fmt.Errorf("accesskey: delete the row of %s: %w", accessKey, err)
	}
	s.logger.InfoContext(ctx, "accesskey: deleted", "access_key", accessKey)
	return nil
}

// present is the set of access keys RustFS has.
func (s *Service) present(ctx context.Context) (map[string]bool, error) {
	ids, err := s.objects.ListAccessKeys(ctx)
	if err != nil {
		return nil, mapErr(err)
	}
	present := make(map[string]bool, len(ids))
	for _, id := range ids {
		present[id] = true
	}
	return present, nil
}

// nameFree refuses a name another key (not except) already has.
func (s *Service) nameFree(ctx context.Context, name, except string) error {
	rows, err := s.store.ListAccessKeyRows(ctx)
	if err != nil {
		return fmt.Errorf("accesskey: list the rows: %w", err)
	}
	for _, row := range rows {
		if row.Name == name && row.AccessKey != except {
			return fmt.Errorf("%w: an access key named %q already exists", service.ErrAlreadyExists, name)
		}
	}
	return nil
}

// validateBuckets checks the grant names existing buckets, so a typo is a
// field error rather than a key that silently reaches nothing.
func (s *Service) validateBuckets(ctx context.Context, buckets []string) []service.FieldError {
	if len(buckets) == 0 || len(buckets) > MaxBuckets {
		return []service.FieldError{{Field: "buckets", Reason: fmt.Sprintf("must name 1-%d buckets", MaxBuckets)}}
	}
	for _, b := range buckets {
		if _, err := s.objects.HeadBucket(ctx, b); err != nil {
			reason := fmt.Sprintf("no bucket %q", b)
			if !errors.Is(err, objectstore.ErrNotFound) {
				reason = fmt.Sprintf("bucket %q could not be checked: %v", b, err)
			}
			return []service.FieldError{{Field: "buckets", Reason: reason}}
		}
	}
	return nil
}

func validateName(name string) []service.FieldError {
	if !namePattern.MatchString(name) {
		return []service.FieldError{{Field: "name", Reason: fmt.Sprintf(
			"must be 1-%d letters, digits, '.', '_' or '-', starting with a letter or digit", MaxNameLength)}}
	}
	return nil
}

func validateLevel(level objectstore.AccessLevel) []service.FieldError {
	if level != objectstore.AccessReadOnly && level != objectstore.AccessReadWrite {
		return []service.FieldError{{Field: "level", Reason: "must be read-only or read-write"}}
	}
	return nil
}

// normalise sorts a bucket list and drops repeats.
func normalise(buckets []string) []string {
	out := slices.Clone(buckets)
	slices.Sort(out)
	return slices.Compact(out)
}

func setBuckets(ctx context.Context, tx *store.Tx, accessKey string, buckets []string) error {
	if err := tx.ClearAccessKeyBuckets(ctx, sqlc.ClearAccessKeyBucketsParams{AccessKey: accessKey}); err != nil {
		return fmt.Errorf("accesskey: clear the grant of %s: %w", accessKey, err)
	}
	for _, b := range buckets {
		if err := tx.AddAccessKeyBucket(ctx, sqlc.AddAccessKeyBucketParams{AccessKey: accessKey, Bucket: b}); err != nil {
			return fmt.Errorf("accesskey: grant %s on %s: %w", accessKey, b, err)
		}
	}
	return nil
}

// generate makes a key id (KUMO + 16 base32 characters, 80 random bits, the
// shape of an AWS key id) and a 40-character secret (240 random bits).
func generate() (accessKey, secret string, err error) {
	idBytes := make([]byte, 10)
	secretBytes := make([]byte, 30)
	if _, err := rand.Read(idBytes); err != nil {
		return "", "", fmt.Errorf("accesskey: generate the id: %w", err)
	}
	if _, err := rand.Read(secretBytes); err != nil {
		return "", "", fmt.Errorf("accesskey: generate the secret: %w", err)
	}
	return accessKeyPrefix + base32.StdEncoding.EncodeToString(idBytes),
		base64.RawURLEncoding.EncodeToString(secretBytes), nil
}

func fromRow(row sqlc.GetAccessKeyRowRow, buckets []string, present bool) AccessKey {
	key := AccessKey{
		AccessKey: row.AccessKey,
		Name:      row.Name,
		Level:     objectstore.AccessLevel(row.Level),
		Buckets:   buckets,
		State:     StateActive,
		CreatedAt: parseTime(row.CreatedAt),
		UpdatedAt: parseTime(row.UpdatedAt),
	}
	if key.Buckets == nil {
		key.Buckets = []string{}
	}
	if !present {
		key.State = StateMissing
	}
	if row.CreatedByUsername.Valid {
		username := row.CreatedByUsername.String
		key.CreatedBy = &username
	}
	return key
}

func mapErr(err error) error {
	switch {
	case errors.Is(err, objectstore.ErrNotFound):
		return fmt.Errorf("%w: %w", service.ErrNotFound, err)
	case errors.Is(err, objectstore.ErrUnavailable):
		return fmt.Errorf("%w: the object store: %w", service.ErrUnavailable, err)
	default:
		return fmt.Errorf("accesskey: %w", err)
	}
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parseTime(v string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}
