package keypair

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/id"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
)

// Type is the key algorithm of a key pair. The values mirror the CHECK
// constraint on key_pairs.type (docs/design/02-data-model.md).
type Type string

// The supported key algorithms. `rsa` generates as rsa-4096 per 01-api.md.
const (
	TypeED25519 Type = "ed25519"
	TypeRSA     Type = "rsa"
)

// SortOrder selects the order of a key pair page. Only the created_at
// orders of 01-api.md §Conventions are supported.
type SortOrder string

// The supported sort orders.
const (
	SortCreatedDesc SortOrder = "created_at:desc"
	SortCreatedAsc  SortOrder = "created_at:asc"
)

// Limits from 01-api.md §Key pairs, the tag rules of 02-data-model.md and
// the pagination bounds of api/openapi.yaml.
const (
	maxNameLength = 64
	maxTags       = 50
	maxTagKey     = 128
	maxTagValue   = 256

	// MinPageSize, DefaultPageSize and MaxPageSize bound ListInput.PageSize;
	// zero means "the default".
	MinPageSize     = 1
	DefaultPageSize = 50
	MaxPageSize     = 200
)

// namePattern is the key pair name rule of 01-api.md.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// KeyPair is the safe projection of a key pair row: the private half never
// appears here. Tags are always present, even when empty.
type KeyPair struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Type        Type              `json:"type"`
	Fingerprint string            `json:"fingerprint"`
	PublicKey   string            `json:"public_key"`
	Imported    bool              `json:"-"`
	CreatedAt   time.Time         `json:"created_at"`
	Tags        map[string]string `json:"tags"`
}

// Detail is a key pair plus the live instances that were launched with it.
type Detail struct {
	KeyPair
	UsedBy []string `json:"used_by"`
}

// Created is the answer to POST /key-pairs: the only place the private key
// ever leaves the process. Nothing in this package logs or persists it.
type Created struct {
	KeyPair
	PrivateKey string `json:"private_key"`
}

// Page is one page of a key pair listing.
type Page struct {
	Items         []KeyPair `json:"items"`
	NextPageToken *string   `json:"next_page_token"`
}

// ListInput selects a page of key pairs.
type ListInput struct {
	Query     string
	Sort      SortOrder
	PageSize  int
	PageToken string
}

// CreateInput is a POST /key-pairs body with the calling user attached.
type CreateInput struct {
	Name      string
	Type      Type
	Tags      map[string]string
	CreatedBy string
}

// ImportInput is a POST /key-pairs/import body with the calling user
// attached.
type ImportInput struct {
	Name      string
	PublicKey string
	Tags      map[string]string
	CreatedBy string
}

// Options configures the service.
type Options struct {
	Logger *slog.Logger
	Store  *store.Store
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Sentinel errors of the constructor, so a caller can tell a wiring mistake
// from a runtime failure.
var (
	// ErrNoLogger means Options.Logger was nil.
	ErrNoLogger = errors.New("keypair: a logger is required")
	// ErrNoStore means Options.Store was nil.
	ErrNoStore = errors.New("keypair: a store is required")
)

// Service owns key pairs.
type Service struct {
	logger *slog.Logger
	store  *store.Store
	now    func() time.Time
}

// New builds the service.
func New(opts Options) (*Service, error) {
	if opts.Logger == nil {
		return nil, ErrNoLogger
	}
	if opts.Store == nil {
		return nil, ErrNoStore
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}

	return &Service{logger: opts.Logger, store: opts.Store, now: opts.Now}, nil
}

// Create generates a new key pair. The returned Created carries the private
// key, which never reaches the database or the logs; losing it means
// generating a new pair.
func (s *Service) Create(ctx context.Context, in CreateInput) (Created, error) {
	fields := validateNameTags(in.Name, in.Tags)
	switch in.Type {
	case "", TypeED25519, TypeRSA:
		// The empty type takes the API default.
	default:
		fields = append(fields, service.FieldError{Field: "type", Reason: `must be "ed25519" or "rsa"`})
	}
	if len(fields) > 0 {
		return Created{}, &service.ValidationError{Message: "the key pair is not acceptable", Fields: fields}
	}
	if in.Type == "" {
		in.Type = TypeED25519
	}

	material, err := generate(in.Type, in.Name)
	if err != nil {
		return Created{}, err
	}

	pair, err := s.insert(ctx, input{
		name:        in.Name,
		keyType:     in.Type,
		publicLine:  material.publicLine,
		fingerprint: material.fingerprint,
		tags:        in.Tags,
		createdBy:   in.CreatedBy,
	})
	if err != nil {
		return Created{}, err
	}

	s.logger.InfoContext(ctx, "keypair: generated",
		"key_pair_id", pair.ID, "name", pair.Name, "type", pair.Type)

	return Created{KeyPair: pair, PrivateKey: material.privatePEM}, nil
}

// Import registers an existing public key. Only the public half is ever
// stored; pasted private key material is rejected before parsing.
func (s *Service) Import(ctx context.Context, in ImportInput) (KeyPair, error) {
	if fields := validateNameTags(in.Name, in.Tags); len(fields) > 0 {
		return KeyPair{}, &service.ValidationError{Message: "the key pair is not acceptable", Fields: fields}
	}

	publicLine, keyType, fingerprint, err := parsePublicKey(in.PublicKey)
	if err != nil {
		return KeyPair{}, err
	}

	pair, err := s.insert(ctx, input{
		name:        in.Name,
		keyType:     keyType,
		publicLine:  publicLine,
		fingerprint: fingerprint,
		tags:        in.Tags,
		createdBy:   in.CreatedBy,
		imported:    true,
	})
	if err != nil {
		return KeyPair{}, err
	}

	s.logger.InfoContext(ctx, "keypair: imported",
		"key_pair_id", pair.ID, "name", pair.Name, "type", pair.Type)

	return pair, nil
}

// List returns one page of key pairs.
func (s *Service) List(ctx context.Context, in ListInput) (Page, error) {
	if in.Sort == "" {
		in.Sort = SortCreatedDesc
	}
	switch in.Sort {
	case SortCreatedAsc, SortCreatedDesc:
	default:
		return Page{}, &service.ValidationError{
			Message: "the sort order is not acceptable",
			Fields: []service.FieldError{{
				Field:  "sort",
				Reason: `must be "created_at:asc" or "created_at:desc"`,
			}},
		}
	}

	if in.PageSize == 0 {
		in.PageSize = DefaultPageSize
	}
	if in.PageSize < MinPageSize || in.PageSize > MaxPageSize {
		return Page{}, &service.ValidationError{
			Message: "the page size is not acceptable",
			Fields: []service.FieldError{{
				Field:  "page_size",
				Reason: fmt.Sprintf("must be between %d and %d", MinPageSize, MaxPageSize),
			}},
		}
	}

	cursor, err := decodePageToken(in.PageToken)
	if err != nil {
		return Page{}, err
	}

	rows, err := s.listPage(ctx, in, cursor)
	if err != nil {
		return Page{}, fmt.Errorf("list key pairs: %w", err)
	}

	page := Page{Items: make([]KeyPair, 0, min(len(rows), in.PageSize))}
	if len(rows) > in.PageSize {
		last := rows[in.PageSize-1]
		token := encodePageToken(last.CreatedAt, last.ID)
		page.NextPageToken = &token
		rows = rows[:in.PageSize]
	}

	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	tags, err := s.tagsByResource(ctx, ids)
	if err != nil {
		return Page{}, err
	}
	for _, row := range rows {
		page.Items = append(page.Items, keyPairFromRow(row, tags[row.ID]))
	}

	return page, nil
}

// Get returns one key pair with the live instances launched with it.
func (s *Service) Get(ctx context.Context, keyPairID string) (Detail, error) {
	row, err := s.store.GetKeyPair(ctx, sqlc.GetKeyPairParams{ID: keyPairID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Detail{}, fmt.Errorf("%w: no key pair %q", service.ErrNotFound, keyPairID)
		}
		return Detail{}, fmt.Errorf("get key pair: %w", err)
	}

	tags, err := s.tagsByResource(ctx, []string{row.ID})
	if err != nil {
		return Detail{}, err
	}

	usedBy, err := s.store.ListLiveInstancesWithKeyPair(ctx,
		sqlc.ListLiveInstancesWithKeyPairParams{KeyPairID: nullable(row.ID)})
	if err != nil {
		return Detail{}, fmt.Errorf("list instances using key pair: %w", err)
	}

	return Detail{
		KeyPair: keyPairFromRow(row, tags[row.ID]),
		UsedBy:  usedBy,
	}, nil
}

// UpdateTags replaces the tag set of a key pair.
func (s *Service) UpdateTags(ctx context.Context, keyPairID string, tags map[string]string) (KeyPair, error) {
	if field := validateTags(tags); field != nil {
		return KeyPair{}, &service.ValidationError{
			Message: "the tags are not acceptable",
			Fields:  []service.FieldError{*field},
		}
	}

	var pair KeyPair
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		row, err := tx.GetKeyPair(ctx, sqlc.GetKeyPairParams{ID: keyPairID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no key pair %q", service.ErrNotFound, keyPairID)
			}
			return fmt.Errorf("get key pair: %w", err)
		}

		if err := replaceTags(ctx, tx, row.ID, tags); err != nil {
			return err
		}

		pair = keyPairFromRow(row, tags)
		return nil
	})
	if err != nil {
		return KeyPair{}, err
	}

	return pair, nil
}

// Delete removes a key pair and its tags. As in AWS, a key in use by a
// non-terminated instance cannot be deleted unless force is set; the running
// instances keep the keys already injected into them
// (docs/design/01-api.md §Key pairs).
func (s *Service) Delete(ctx context.Context, keyPairID string, force bool) error {
	var name string
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		row, err := tx.GetKeyPair(ctx, sqlc.GetKeyPairParams{ID: keyPairID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: no key pair %q", service.ErrNotFound, keyPairID)
			}
			return fmt.Errorf("get key pair: %w", err)
		}

		if !force {
			users, err := tx.ListLiveInstancesWithKeyPair(ctx,
				sqlc.ListLiveInstancesWithKeyPairParams{KeyPairID: nullable(row.ID)})
			if err != nil {
				return fmt.Errorf("list instances using key pair: %w", err)
			}
			if len(users) > 0 {
				return fmt.Errorf("%w: key pair %q is in use by %s",
					service.ErrConflict, row.Name, strings.Join(users, ", "))
			}
		}

		if err := tx.DeleteResourceTags(ctx, sqlc.DeleteResourceTagsParams{ResourceID: row.ID}); err != nil {
			return fmt.Errorf("delete key pair tags: %w", err)
		}
		if _, err := tx.DeleteKeyPair(ctx, sqlc.DeleteKeyPairParams{ID: row.ID}); err != nil {
			return fmt.Errorf("delete key pair: %w", err)
		}

		name = row.Name
		return nil
	})
	if err != nil {
		return err
	}

	s.logger.InfoContext(ctx, "keypair: deleted", "key_pair_id", keyPairID, "name", name)
	return nil
}

// input is one stored key pair before it has an ID.
type input struct {
	name        string
	keyType     Type
	publicLine  string
	fingerprint string
	tags        map[string]string
	createdBy   string
	imported    bool
}

// insert stores a key pair and its tags in one transaction. The name is
// unique; a taken name maps to ErrAlreadyExists.
func (s *Service) insert(ctx context.Context, in input) (KeyPair, error) {
	var pair KeyPair
	err := s.store.WithTx(ctx, func(ctx context.Context, tx *store.Tx) error {
		row, err := tx.CreateKeyPair(ctx, sqlc.CreateKeyPairParams{
			ID:          id.KeyPair(),
			Name:        in.name,
			Type:        string(in.keyType),
			PublicKey:   in.publicLine,
			Fingerprint: in.fingerprint,
			Imported:    boolToInt(in.imported),
			CreatedBy:   nullable(in.createdBy),
			CreatedAt:   formatTime(s.now()),
		})
		if err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("%w: a key pair named %q already exists", service.ErrAlreadyExists, in.name)
			}
			return fmt.Errorf("create key pair: %w", err)
		}

		if err := replaceTags(ctx, tx, row.ID, in.tags); err != nil {
			return err
		}

		pair = keyPairFromRow(row, in.tags)
		return nil
	})
	if err != nil {
		return KeyPair{}, err
	}

	return pair, nil
}

// listPage runs the asc or desc variant of the listing query and returns at
// most PageSize+1 rows; the extra row is how the caller learns there is a
// next page.
func (s *Service) listPage(ctx context.Context, in ListInput, cursor pageCursor) ([]sqlc.KeyPair, error) {
	if in.Sort == SortCreatedAsc {
		return s.store.ListKeyPairsAsc(ctx, sqlc.ListKeyPairsAscParams{
			Query:         in.Query,
			CursorCreated: cursor.created,
			CursorID:      cursor.id,
			PageLimit:     int64(in.PageSize) + 1,
		})
	}
	return s.store.ListKeyPairsDesc(ctx, sqlc.ListKeyPairsDescParams{
		Query:         in.Query,
		CursorCreated: cursor.created,
		CursorID:      cursor.id,
		PageLimit:     int64(in.PageSize) + 1,
	})
}

// tagsByResource loads the tags of every given resource id in one query.
func (s *Service) tagsByResource(ctx context.Context, ids []string) (map[string]map[string]string, error) {
	grouped := make(map[string]map[string]string, len(ids))
	if len(ids) == 0 {
		return grouped, nil
	}

	rows, err := s.store.ListTagsForResources(ctx, sqlc.ListTagsForResourcesParams{ResourceIds: ids})
	if err != nil {
		return nil, fmt.Errorf("list tags: %w", err)
	}
	for _, row := range rows {
		if grouped[row.ResourceID] == nil {
			grouped[row.ResourceID] = map[string]string{}
		}
		grouped[row.ResourceID][row.Key] = row.Value
	}
	return grouped, nil
}

// replaceTags swaps the whole tag set of a resource. The caller runs it
// inside a transaction.
func replaceTags(ctx context.Context, tx *store.Tx, resourceID string, tags map[string]string) error {
	if err := tx.DeleteResourceTags(ctx, sqlc.DeleteResourceTagsParams{ResourceID: resourceID}); err != nil {
		return fmt.Errorf("delete tags: %w", err)
	}
	for key, value := range tags {
		if err := tx.SetTag(ctx, sqlc.SetTagParams{ResourceID: resourceID, Key: key, Value: value}); err != nil {
			return fmt.Errorf("set tag %q: %w", key, err)
		}
	}
	return nil
}

// pageCursor is the position a page token encodes.
type pageCursor struct {
	created string
	id      string
}

// encodePageToken packs the cursor of a row into an opaque token.
func encodePageToken(createdAt, keyPairID string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "|" + keyPairID))
}

// decodePageToken unpacks a token; an empty token means the first page.
func decodePageToken(token string) (pageCursor, error) {
	if token == "" {
		return pageCursor{}, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return pageCursor{}, invalidPageToken()
	}
	created, keyPairID, ok := strings.Cut(string(raw), "|")
	if !ok || created == "" || !id.HasResourceShape(id.PrefixKeyPair, keyPairID) {
		return pageCursor{}, invalidPageToken()
	}
	return pageCursor{created: created, id: keyPairID}, nil
}

func invalidPageToken() *service.ValidationError {
	return &service.ValidationError{
		Message: "the page token is not acceptable",
		Fields:  []service.FieldError{{Field: "page_token", Reason: "is not a token this API issued"}},
	}
}

// validateNameTags checks the fields create and import share.
func validateNameTags(name string, tags map[string]string) []service.FieldError {
	var fields []service.FieldError
	if name == "" {
		fields = append(fields, service.FieldError{Field: "name", Reason: "must not be empty"})
	} else if !namePattern.MatchString(name) {
		fields = append(fields, service.FieldError{
			Field: "name",
			Reason: fmt.Sprintf(
				"must be 1-%d characters of letters, digits, '.', '_' or '-'", maxNameLength),
		})
	}
	if err := validateTags(tags); err != nil {
		fields = append(fields, *err)
	}
	return fields
}

// validateTags enforces the tag limits of 02-data-model.md.
func validateTags(tags map[string]string) *service.FieldError {
	if len(tags) > maxTags {
		return &service.FieldError{
			Field: "tags", Reason: fmt.Sprintf("at most %d tags per resource", maxTags),
		}
	}
	for key, value := range tags {
		if key == "" {
			return &service.FieldError{Field: "tags", Reason: "tag keys must not be empty"}
		}
		if len(key) > maxTagKey || len(value) > maxTagValue {
			return &service.FieldError{
				Field: "tags",
				Reason: fmt.Sprintf("tag keys are at most %d and values at most %d characters",
					maxTagKey, maxTagValue),
			}
		}
	}
	return nil
}

func keyPairFromRow(row sqlc.KeyPair, tags map[string]string) KeyPair {
	createdAt, _ := time.Parse(time.RFC3339Nano, row.CreatedAt) //nolint:errcheck // stored by this package

	if tags == nil {
		tags = map[string]string{}
	}
	return KeyPair{
		ID:          row.ID,
		Name:        row.Name,
		Type:        Type(row.Type),
		Fingerprint: row.Fingerprint,
		PublicKey:   row.PublicKey,
		Imported:    row.Imported != 0,
		CreatedAt:   createdAt.UTC(),
		Tags:        tags,
	}
}

// isUniqueViolation reports a UNIQUE constraint failure. SQLite error codes
// are matched on their text because modernc.org/sqlite does not export a
// typed error for them.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func nullable(value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

func boolToInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}
