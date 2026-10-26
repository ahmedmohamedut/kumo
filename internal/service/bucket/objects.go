package bucket

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	"github.com/ahmedmohamedut/kumo/internal/service"
)

// Limits of the object endpoints (docs/design/06-object-storage.md §4).
const (
	// MaxKeyBytes is the S3 limit on an object key.
	MaxKeyBytes = 1024
	// MaxDeleteKeys is the most keys one delete request removes, the S3 batch.
	MaxDeleteKeys = 1000
	// DefaultListLimit is the page size of a listing when none is asked for.
	DefaultListLimit = 200
	// folderDelimiter is what makes a key prefix read as a folder.
	folderDelimiter = "/"
)

// contentTypePattern is a media type: type/subtype with optional parameters.
var contentTypePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*(\s*;.*)?$`)

// Listing is one page of a bucket's folder view.
type Listing struct {
	Prefix string `json:"prefix"`
	// Folders are the common prefixes one level below Prefix, each ending in "/".
	Folders    []string `json:"folders"`
	Objects    []Object `json:"objects"`
	NextCursor *string  `json:"next_cursor"`
}

// Object is one key in a listing.
type Object struct {
	Key          string    `json:"key"`
	SizeBytes    int64     `json:"size_bytes"`
	LastModified time.Time `json:"last_modified"`
	ETag         string    `json:"etag"`
}

// ListObjectsInput pages through one folder of a bucket.
type ListObjectsInput struct {
	Prefix string
	Cursor string
	Limit  int
}

// Presigned is a URL the browser uses against the object store directly.
type Presigned struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresAt time.Time         `json:"expires_at"`
}

// ListObjects returns one page of the folder at Prefix: the folders directly
// below it and the objects in it. A zero-byte "folder/" marker is a folder,
// not an object, so it is left out of Objects.
func (s *Service) ListObjects(ctx context.Context, name string, in ListObjectsInput) (Listing, error) {
	if in.Prefix != "" {
		if field := validateKey("prefix", in.Prefix); field != nil {
			return Listing{}, invalid(*field)
		}
	}
	limit := in.Limit
	switch {
	case limit == 0:
		limit = DefaultListLimit
	case limit < 1 || limit > objectstore.MaxListLimit:
		return Listing{}, invalid(service.FieldError{
			Field: "limit", Reason: fmt.Sprintf("must be between 1 and %d", objectstore.MaxListLimit),
		})
	}
	if _, err := s.get(ctx, name); err != nil {
		return Listing{}, err
	}

	page, err := s.objects.ListObjects(ctx, name, objectstore.ListInput{
		Prefix: in.Prefix, Delimiter: folderDelimiter, Cursor: in.Cursor, Limit: limit,
	})
	if err != nil {
		return Listing{}, mapErr(err)
	}

	listing := Listing{
		Prefix:  in.Prefix,
		Folders: page.CommonPrefixes,
		Objects: make([]Object, 0, len(page.Objects)),
	}
	for _, o := range page.Objects {
		if o.Key == in.Prefix && strings.HasSuffix(o.Key, folderDelimiter) {
			continue // the marker of the folder being listed
		}
		listing.Objects = append(listing.Objects, Object{
			Key: o.Key, SizeBytes: o.SizeBytes, LastModified: o.LastModified, ETag: o.ETag,
		})
	}
	if page.NextCursor != "" {
		listing.NextCursor = &page.NextCursor
	}
	return listing, nil
}

// PresignUpload returns a URL the browser PUTs one object to. The content
// type is signed into it, so the browser must send the same header.
//
// The bucket's CORS rule is applied first if this process has not done so,
// because a bucket made outside Kumo has none and the browser would refuse
// the upload without it.
func (s *Service) PresignUpload(ctx context.Context, name, key, contentType string) (Presigned, error) {
	var fields []service.FieldError
	if field := validateKey("key", key); field != nil {
		fields = append(fields, *field)
	} else if strings.HasSuffix(key, folderDelimiter) {
		fields = append(fields, service.FieldError{Field: "key", Reason: "must not end in '/'; create a folder instead"})
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if len(contentType) > 255 || !contentTypePattern.MatchString(contentType) {
		fields = append(fields, service.FieldError{Field: "content_type", Reason: "must be a media type such as text/plain"})
	}
	if len(fields) > 0 {
		return Presigned{}, invalid(fields...)
	}
	if err := s.writable(ctx, name); err != nil {
		return Presigned{}, err
	}
	if err := s.ensureCORS(ctx, name); err != nil {
		return Presigned{}, err
	}

	signed, err := s.objects.PresignPut(ctx, name, key, objectstore.PresignPutInput{
		TTL: s.presignTTL, ContentType: contentType,
	})
	if err != nil {
		return Presigned{}, mapErr(err)
	}
	s.logger.InfoContext(ctx, "bucket: upload URL issued", "bucket", name, "key", key)
	return presigned(signed), nil
}

// PresignDownload returns a URL that downloads one object under its base
// name. The object must exist, so a stale row in the browser is a 404 here
// rather than an XML error page from the object store.
func (s *Service) PresignDownload(ctx context.Context, name, key string) (Presigned, error) {
	if field := validateKey("key", key); field != nil {
		return Presigned{}, invalid(*field)
	}
	if _, err := s.get(ctx, name); err != nil {
		return Presigned{}, err
	}
	if _, err := s.objects.HeadObject(ctx, name, key); err != nil {
		if errors.Is(err, objectstore.ErrNotFound) {
			return Presigned{}, fmt.Errorf("%w: no object %q in bucket %q", service.ErrNotFound, key, name)
		}
		return Presigned{}, mapErr(err)
	}

	signed, err := s.objects.PresignGet(ctx, name, key, objectstore.PresignGetInput{
		TTL: s.presignTTL, Filename: path.Base(key),
	})
	if err != nil {
		return Presigned{}, mapErr(err)
	}
	return presigned(signed), nil
}

// CreateFolder writes the zero-byte "prefix/" marker S3 consoles use for an
// empty folder.
func (s *Service) CreateFolder(ctx context.Context, name, prefix string) (string, error) {
	folder := strings.TrimSuffix(prefix, folderDelimiter) + folderDelimiter
	if field := validateKey("prefix", folder); field != nil || folder == folderDelimiter {
		if field == nil {
			field = &service.FieldError{Field: "prefix", Reason: "must name a folder"}
		}
		return "", invalid(*field)
	}
	if err := s.writable(ctx, name); err != nil {
		return "", err
	}
	if err := s.objects.PutObject(ctx, name, folder, bytes.NewReader(nil), 0, "application/x-directory"); err != nil {
		return "", mapErr(err)
	}
	s.logger.InfoContext(ctx, "bucket: folder created", "bucket", name, "prefix", folder)
	return folder, nil
}

// DeleteObjects removes keys. A key that does not exist is not an error. A
// folder key ("photos/") removes only its marker: the objects below it stay,
// so deleting a folder by mistake never takes its contents with it.
func (s *Service) DeleteObjects(ctx context.Context, name string, keys []string) error {
	if len(keys) == 0 || len(keys) > MaxDeleteKeys {
		return invalid(service.FieldError{
			Field: "keys", Reason: fmt.Sprintf("must list between 1 and %d keys", MaxDeleteKeys),
		})
	}
	for _, key := range keys {
		if field := validateKey("keys", key); field != nil {
			return invalid(*field)
		}
	}
	if err := s.writable(ctx, name); err != nil {
		return err
	}
	if err := s.objects.DeleteObjects(ctx, name, keys); err != nil {
		return mapErr(err)
	}
	s.logger.InfoContext(ctx, "bucket: objects deleted", "bucket", name, "count", len(keys))
	return nil
}

// writable checks the bucket exists and is not being deleted, which would
// race the task emptying it.
func (s *Service) writable(ctx context.Context, name string) error {
	b, err := s.get(ctx, name)
	if err != nil {
		return err
	}
	if b.State == StateDeleting {
		return fmt.Errorf("%w: bucket %q is being deleted", service.ErrConflict, name)
	}
	return nil
}

// ensureCORS applies the console's CORS rule once per bucket per process.
// SetCORS keeps an operator's own rules, so applying it again is harmless; the
// cache only saves a request per upload.
func (s *Service) ensureCORS(ctx context.Context, name string) error {
	if len(s.corsOrigins) == 0 {
		return nil
	}
	s.corsMu.Lock()
	ready := s.corsReady[name]
	s.corsMu.Unlock()
	if ready {
		return nil
	}
	if err := s.objects.SetCORS(ctx, name, s.corsOrigins); err != nil {
		return mapErr(err)
	}
	s.markCORSReady(name)
	return nil
}

func (s *Service) markCORSReady(name string) {
	s.corsMu.Lock()
	defer s.corsMu.Unlock()
	s.corsReady[name] = true
}

// validateKey bounds a key or prefix: 1-1024 bytes of UTF-8, no control
// characters, not starting with '/'.
func validateKey(field, key string) *service.FieldError {
	switch {
	case key == "" || len(key) > MaxKeyBytes:
		return &service.FieldError{Field: field, Reason: fmt.Sprintf("must be 1-%d bytes", MaxKeyBytes)}
	case !utf8.ValidString(key):
		return &service.FieldError{Field: field, Reason: "must be valid UTF-8"}
	case strings.HasPrefix(key, "/"):
		return &service.FieldError{Field: field, Reason: "must not start with '/'"}
	case strings.ContainsFunc(key, func(r rune) bool { return r < 0x20 || r == 0x7f }):
		return &service.FieldError{Field: field, Reason: "must not contain control characters"}
	}
	return nil
}

func invalid(fields ...service.FieldError) error {
	return &service.ValidationError{Message: "the request is not acceptable", Fields: fields}
}

func presigned(p objectstore.Presigned) Presigned {
	return Presigned{URL: p.URL, Method: p.Method, Headers: p.Headers, ExpiresAt: p.ExpiresAt}
}
