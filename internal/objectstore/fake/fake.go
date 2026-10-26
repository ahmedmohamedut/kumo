// Package fake is an in-memory objectstore.ObjectStore for tests and for
// KUMO_OBJECTSTORE=fake. It keeps the S3 rules the services rely on (a bucket
// must be empty to delete, versioning cannot go back to off, listings group
// by delimiter and page in key order) so a test against it means something
// against RustFS. The storetest suite runs against both.
package fake

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // an S3 ETag is an MD5, not a security property
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
)

// PublicURL is the address the fake signs its presigned URLs for. Nothing
// listens there; a test reads the URL rather than following it.
const PublicURL = "http://objectstore.fake"

// errBodySize means a PutObject body was not the size the caller declared.
var errBodySize = errors.New("fake: the body is not the declared size")

// Store is the fake backend.
type Store struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	keys    map[string]fakeKey
	now     func() time.Time
	// down makes every call fail with ErrUnavailable, for readiness tests.
	down bool
}

type bucket struct {
	createdAt  time.Time
	versioning objectstore.Versioning
	cors       []string
	objects    map[string]object
}

type object struct {
	data         []byte
	contentType  string
	lastModified time.Time
}

var _ objectstore.ObjectStore = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	return &Store{buckets: map[string]*bucket{}, keys: map[string]fakeKey{}, now: time.Now}
}

// SetDown makes the store answer every call with ErrUnavailable until it is
// set back.
func (s *Store) SetDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

// CORS returns the origins SetCORS last allowed on a bucket, for tests.
func (s *Store) CORS(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b, ok := s.buckets[name]; ok {
		return slices.Clone(b.cors)
	}
	return nil
}

// Ping reports the simulated reachability.
func (s *Store) Ping(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.check("ping")
}

// ListBuckets returns the buckets by name.
func (s *Store) ListBuckets(context.Context) ([]objectstore.Bucket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check("list buckets"); err != nil {
		return nil, err
	}
	out := make([]objectstore.Bucket, 0, len(s.buckets))
	for name, b := range s.buckets {
		out = append(out, objectstore.Bucket{Name: name, CreatedAt: b.createdAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// HeadBucket returns one bucket.
func (s *Store) HeadBucket(_ context.Context, name string) (objectstore.Bucket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket("head bucket", name)
	if err != nil {
		return objectstore.Bucket{}, err
	}
	return objectstore.Bucket{Name: name, CreatedAt: b.createdAt}, nil
}

// CreateBucket adds an empty bucket.
func (s *Store) CreateBucket(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check("create bucket"); err != nil {
		return err
	}
	if name == "" || strings.ContainsAny(name, "/ ") {
		return fmt.Errorf("%w: bucket %q", objectstore.ErrInvalidName, name)
	}
	if _, ok := s.buckets[name]; ok {
		return fmt.Errorf("%w: bucket %q", objectstore.ErrAlreadyExists, name)
	}
	s.buckets[name] = &bucket{
		createdAt:  s.now().UTC().Truncate(time.Second),
		versioning: objectstore.VersioningOff,
		objects:    map[string]object{},
	}
	return nil
}

// DeleteBucket removes an empty bucket.
func (s *Store) DeleteBucket(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket("delete bucket", name)
	if err != nil {
		return err
	}
	if len(b.objects) > 0 {
		return fmt.Errorf("%w: bucket %q", objectstore.ErrBucketNotEmpty, name)
	}
	delete(s.buckets, name)
	return nil
}

// EmptyBucket removes every object. The fake keeps no old versions, so the
// objects are all there is.
func (s *Store) EmptyBucket(_ context.Context, name string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket("empty bucket", name)
	if err != nil {
		return 0, err
	}
	n := len(b.objects)
	b.objects = map[string]object{}
	return n, nil
}

// GetVersioning reads the versioning state.
func (s *Store) GetVersioning(_ context.Context, name string) (objectstore.Versioning, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket("get versioning", name)
	if err != nil {
		return "", err
	}
	return b.versioning, nil
}

// SetVersioning enables or suspends versioning. The fake keeps one version of
// each key regardless; only the state is simulated.
func (s *Store) SetVersioning(_ context.Context, name string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket("set versioning", name)
	if err != nil {
		return err
	}
	if enabled {
		b.versioning = objectstore.VersioningEnabled
	} else {
		b.versioning = objectstore.VersioningSuspended
	}
	return nil
}

// SetCORS records the allowed origins.
func (s *Store) SetCORS(_ context.Context, name string, origins []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket("set CORS", name)
	if err != nil {
		return err
	}
	b.cors = slices.Clone(origins)
	return nil
}

// ListObjects pages through the keys under a prefix in key order, grouping by
// the delimiter the way S3 does. The cursor is the last key or prefix of the
// previous page.
func (s *Store) ListObjects(_ context.Context, name string, in objectstore.ListInput) (objectstore.ObjectPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket("list objects", name)
	if err != nil {
		return objectstore.ObjectPage{}, err
	}
	limit := in.Limit
	if limit <= 0 || limit > objectstore.MaxListLimit {
		limit = objectstore.MaxListLimit
	}

	// Build the entries of this level: objects, and prefixes standing in for
	// every key below them.
	type entry struct {
		name   string
		prefix bool
	}
	seen := map[string]bool{}
	var entries []entry
	for key := range b.objects {
		if !strings.HasPrefix(key, in.Prefix) {
			continue
		}
		rest := key[len(in.Prefix):]
		if in.Delimiter != "" {
			if i := strings.Index(rest, in.Delimiter); i >= 0 {
				p := in.Prefix + rest[:i+len(in.Delimiter)]
				if !seen[p] {
					seen[p] = true
					entries = append(entries, entry{name: p, prefix: true})
				}
				continue
			}
		}
		entries = append(entries, entry{name: key})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	page := objectstore.ObjectPage{Objects: []objectstore.Object{}, CommonPrefixes: []string{}}
	count := 0
	for i, e := range entries {
		if in.Cursor != "" && e.name <= in.Cursor {
			continue
		}
		if count == limit {
			page.NextCursor = entries[i-1].name
			break
		}
		count++
		if e.prefix {
			page.CommonPrefixes = append(page.CommonPrefixes, e.name)
			continue
		}
		o := info(e.name, b.objects[e.name])
		o.ContentType = "" // an S3 listing does not carry it
		page.Objects = append(page.Objects, o)
	}
	return page, nil
}

// HeadObject reads one object's metadata.
func (s *Store) HeadObject(_ context.Context, name, key string) (objectstore.Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket("head object", name)
	if err != nil {
		return objectstore.Object{}, err
	}
	o, ok := b.objects[key]
	if !ok {
		return objectstore.Object{}, fmt.Errorf("%w: object %s/%s", objectstore.ErrNotFound, name, key)
	}
	return info(key, o), nil
}

// PutObject stores a body.
func (s *Store) PutObject(_ context.Context, name, key string, r io.Reader, size int64, contentType string) error {
	data, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil {
		return fmt.Errorf("fake: put %s/%s: %w", name, key, err)
	}
	if int64(len(data)) != size {
		return fmt.Errorf("%w: put %s/%s: %d bytes, not %d", errBodySize, name, key, len(data), size)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket("put object", name)
	if err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("%w: an empty key", objectstore.ErrInvalidName)
	}
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	b.objects[key] = object{
		data:         bytes.Clone(data),
		contentType:  contentType,
		lastModified: s.now().UTC().Truncate(time.Second),
	}
	return nil
}

// DeleteObjects removes keys; a missing key is not an error.
func (s *Store) DeleteObjects(_ context.Context, name string, keys []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := s.bucket("delete objects", name)
	if err != nil {
		return err
	}
	for _, key := range keys {
		delete(b.objects, key)
	}
	return nil
}

// PresignGet returns a URL in the shape of a SigV4 one, without a real
// signature.
func (s *Store) PresignGet(_ context.Context, name, key string, in objectstore.PresignGetInput) (objectstore.Presigned, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.bucket("presign get", name); err != nil {
		return objectstore.Presigned{}, err
	}
	q := url.Values{"X-Amz-Expires": {strconv.Itoa(int(in.TTL.Seconds()))}}
	if in.Filename != "" {
		q.Set("response-content-disposition", "attachment; filename=\""+in.Filename+"\"")
	}
	return s.presigned("GET", name, key, q, nil, in.TTL), nil
}

// PresignPut returns an upload URL in the shape of a SigV4 one.
func (s *Store) PresignPut(_ context.Context, name, key string, in objectstore.PresignPutInput) (objectstore.Presigned, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.bucket("presign put", name); err != nil {
		return objectstore.Presigned{}, err
	}
	headers := map[string]string{}
	if in.ContentType != "" {
		headers["Content-Type"] = in.ContentType
	}
	q := url.Values{"X-Amz-Expires": {strconv.Itoa(int(in.TTL.Seconds()))}}
	return s.presigned("PUT", name, key, q, headers, in.TTL), nil
}

func (s *Store) presigned(method, name, key string, q url.Values, headers map[string]string, ttl time.Duration) objectstore.Presigned {
	if headers == nil {
		headers = map[string]string{}
	}
	return objectstore.Presigned{
		URL:       PublicURL + "/" + url.PathEscape(name) + "/" + escapeKey(key) + "?" + q.Encode(),
		Method:    method,
		Headers:   headers,
		ExpiresAt: s.now().Add(ttl).UTC(),
	}
}

// check fails every call while the store is down. The caller holds mu.
func (s *Store) check(op string) error {
	if s.down {
		return fmt.Errorf("%w: %s: the fake store is down", objectstore.ErrUnavailable, op)
	}
	return nil
}

// bucket looks a bucket up. The caller holds mu.
func (s *Store) bucket(op, name string) (*bucket, error) {
	if err := s.check(op); err != nil {
		return nil, err
	}
	b, ok := s.buckets[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s: bucket %q", objectstore.ErrNotFound, op, name)
	}
	return b, nil
}

func info(key string, o object) objectstore.Object {
	sum := md5.Sum(o.data) //nolint:gosec // see the import
	return objectstore.Object{
		Key:          key,
		SizeBytes:    int64(len(o.data)),
		LastModified: o.lastModified,
		ETag:         hex.EncodeToString(sum[:]),
		ContentType:  o.contentType,
	}
}

// escapeKey escapes each path segment of a key, keeping the slashes.
func escapeKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// fakeKey is an access key the fake holds; the secret is kept so a test can
// check it was passed through, never returned.
type fakeKey struct {
	secret string
	grant  objectstore.Grant
}

// CreateAccessKey records a key.
func (s *Store) CreateAccessKey(_ context.Context, spec objectstore.AccessKeySpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check("create access key"); err != nil {
		return err
	}
	if spec.AccessKey == "" || spec.SecretKey == "" || len(spec.Grant.Buckets) == 0 {
		return fmt.Errorf("%w: an access key needs an id, a secret and a bucket", objectstore.ErrInvalidName)
	}
	if _, ok := s.keys[spec.AccessKey]; ok {
		return fmt.Errorf("%w: access key %q", objectstore.ErrAlreadyExists, spec.AccessKey)
	}
	s.keys[spec.AccessKey] = fakeKey{secret: spec.SecretKey, grant: cloneGrant(spec.Grant)}
	return nil
}

// SetAccessKeyPolicy replaces a key's grant.
func (s *Store) SetAccessKeyPolicy(_ context.Context, accessKey string, grant objectstore.Grant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check("set access key policy"); err != nil {
		return err
	}
	key, ok := s.keys[accessKey]
	if !ok {
		return fmt.Errorf("%w: access key %q", objectstore.ErrNotFound, accessKey)
	}
	key.grant = cloneGrant(grant)
	s.keys[accessKey] = key
	return nil
}

// DeleteAccessKey forgets a key.
func (s *Store) DeleteAccessKey(_ context.Context, accessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check("delete access key"); err != nil {
		return err
	}
	delete(s.keys, accessKey)
	return nil
}

// ListAccessKeys returns the key ids, sorted.
func (s *Store) ListAccessKeys(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check("list access keys"); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// AccessKey returns what the fake holds for a key, for tests.
func (s *Store) AccessKey(accessKey string) (secret string, grant objectstore.Grant, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.keys[accessKey]
	return key.secret, cloneGrant(key.grant), ok
}

func cloneGrant(g objectstore.Grant) objectstore.Grant {
	return objectstore.Grant{Buckets: slices.Clone(g.Buckets), Level: g.Level}
}
