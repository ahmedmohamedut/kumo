// Package rustfs implements objectstore.ObjectStore against RustFS, or any
// server that speaks the S3 API, through aws-sdk-go-v2.
//
// Two clients share the credentials: one talks to Endpoint, the address kumod
// reaches the server at, and one signs presigned URLs for PublicURL, the
// address a browser reaches it at. They differ when RustFS listens on
// loopback behind a proxy, and a SigV4 signature covers the host, so a URL
// signed for the wrong one is refused. See docs/design/06-object-storage.md.
package rustfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
)

const (
	// maxPutBytes bounds PutObject, which buffers the body: it is for folder
	// markers and tests, and an upload of any size goes through PresignPut.
	maxPutBytes = 5 << 20
	// requestTimeout bounds one control-plane request. Object bytes never pass
	// through this client, so nothing it sends should take longer.
	requestTimeout = 30 * time.Second
	// maxPresignTTL is the SigV4 ceiling on a presigned URL's lifetime.
	maxPresignTTL = 7 * 24 * time.Hour
)

// Sentinel errors of the constructor, so a caller can tell a wiring mistake
// from a runtime failure.
var (
	// ErrNoEndpoint means Options.Endpoint is not an absolute http(s) URL.
	ErrNoEndpoint = errors.New("rustfs: an http(s) endpoint is required")
	// ErrNoCredentials means the access key or the secret key is empty.
	ErrNoCredentials = errors.New("rustfs: an access key and a secret key are required")
	// ErrPutTooLarge means a PutObject body is over maxPutBytes.
	ErrPutTooLarge = errors.New("rustfs: too large for PutObject; use a presigned upload")
	// ErrPresignTTL means a presigned URL lifetime is outside what SigV4 allows.
	ErrPresignTTL = errors.New("rustfs: presigned URL lifetime out of range")
	// ErrServer is an S3 error with no objectstore equivalent; the server's
	// code and message follow it.
	ErrServer = errors.New("rustfs: server error")
)

// Options configures the client. The secret key is held in memory only and
// never logged.
type Options struct {
	// Endpoint is where kumod reaches the server, e.g. http://127.0.0.1:9000.
	Endpoint string
	// PublicURL is where a browser reaches it; empty means Endpoint.
	PublicURL string
	// Region is signed into every request; RustFS accepts any value but the
	// client must agree with itself.
	Region    string
	AccessKey string
	SecretKey string
	// HTTPClient overrides the transport, for tests.
	HTTPClient *http.Client
}

// Store is the RustFS backend.
type Store struct {
	api     *s3.Client
	presign *s3.PresignClient
	now     func() time.Time

	// The admin API (admin.go) is called by hand: aws-sdk-go-v2 has no client
	// for it. The secret key also encrypts the bodies that carry credentials.
	admin    aws.Credentials
	endpoint string
	region   string
	http     *http.Client
}

var _ objectstore.ObjectStore = (*Store)(nil)

// New builds the client. It does not contact the server; Ping does.
func New(opts Options) (*Store, error) {
	if err := checkURL(opts.Endpoint); err != nil {
		return nil, fmt.Errorf("endpoint: %w", err)
	}
	public := opts.PublicURL
	if public == "" {
		public = opts.Endpoint
	}
	if err := checkURL(public); err != nil {
		return nil, fmt.Errorf("public URL: %w", err)
	}
	if opts.AccessKey == "" || opts.SecretKey == "" {
		return nil, ErrNoCredentials
	}
	region := opts.Region
	if region == "" {
		region = "us-east-1"
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}

	creds := aws.Credentials{
		AccessKeyID:     opts.AccessKey,
		SecretAccessKey: opts.SecretKey,
		Source:          "kumo",
	}
	clientFor := func(endpoint string) *s3.Client {
		return s3.New(s3.Options{
			Region:       region,
			BaseEndpoint: aws.String(strings.TrimRight(endpoint, "/")),
			// RustFS serves buckets as paths; virtual-host addressing would need
			// a wildcard DNS record per node.
			UsePathStyle: true,
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return creds, nil
			}),
			// Since early 2025 the SDK adds CRC32 trailers to every upload by
			// default, which S3-compatible servers do not all accept, and which
			// would also be signed into presigned PUT URLs a browser cannot
			// satisfy. Checksums are sent only where the API requires them.
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
			ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
			HTTPClient:                 httpClient,
			// One retry covers a dropped connection; more would hold a readiness
			// probe or a console request for seconds on a server that is down.
			RetryMaxAttempts: 2,
		})
	}

	return &Store{
		api:      clientFor(opts.Endpoint),
		presign:  s3.NewPresignClient(clientFor(public)),
		now:      time.Now,
		admin:    creds,
		endpoint: strings.TrimRight(opts.Endpoint, "/"),
		region:   region,
		http:     httpClient,
	}, nil
}

// checkURL accepts an absolute http or https URL with a host and no query.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoEndpoint, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: %q is not absolute", ErrNoEndpoint, raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: %q must not carry a query or a fragment", ErrNoEndpoint, raw)
	}
	return nil
}

// Ping lists the buckets, which needs both a reachable server and valid
// credentials.
func (s *Store) Ping(ctx context.Context) error {
	if _, err := s.api.ListBuckets(ctx, &s3.ListBucketsInput{}); err != nil {
		return mapErr(err, "ping")
	}
	return nil
}

// ListBuckets returns every bucket, sorted by the server (by name).
func (s *Store) ListBuckets(ctx context.Context) ([]objectstore.Bucket, error) {
	var buckets []objectstore.Bucket
	paginator := s3.NewListBucketsPaginator(s.api, &s3.ListBucketsInput{})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, mapErr(err, "list buckets")
		}
		for _, b := range page.Buckets {
			buckets = append(buckets, objectstore.Bucket{
				Name:      aws.ToString(b.Name),
				CreatedAt: aws.ToTime(b.CreationDate).UTC(),
			})
		}
	}
	return buckets, nil
}

// HeadBucket checks the bucket exists, then reads its creation date from the
// listing, the only place S3 reports it.
func (s *Store) HeadBucket(ctx context.Context, name string) (objectstore.Bucket, error) {
	if _, err := s.api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)}); err != nil {
		return objectstore.Bucket{}, mapErr(err, "head bucket "+name)
	}
	buckets, err := s.ListBuckets(ctx)
	if err != nil {
		return objectstore.Bucket{}, err
	}
	for _, b := range buckets {
		if b.Name == name {
			return b, nil
		}
	}
	// Deleted between the two calls.
	return objectstore.Bucket{}, fmt.Errorf("%w: bucket %q", objectstore.ErrNotFound, name)
}

// CreateBucket creates a bucket; a taken name is ErrAlreadyExists, whether
// Kumo's credentials own it or not. RustFS, like AWS in us-east-1, answers
// 200 to a create of a bucket the caller already owns, so the bucket is
// looked up first. Any answer but "it exists" falls through to the create,
// which reports the real problem; two creates racing past the check both
// succeed, which leaves one bucket and is harmless.
func (s *Store) CreateBucket(ctx context.Context, name string) error {
	if _, err := s.api.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(name)}); err == nil {
		return fmt.Errorf("%w: bucket %q", objectstore.ErrAlreadyExists, name)
	}
	if _, err := s.api.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name)}); err != nil {
		return mapErr(err, "create bucket "+name)
	}
	return nil
}

// DeleteBucket removes an empty bucket.
func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	if _, err := s.api.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String(name)}); err != nil {
		return mapErr(err, "delete bucket "+name)
	}
	return nil
}

// EmptyBucket deletes every version and delete marker in the bucket, a page
// of up to 1000 at a time. On an unversioned bucket the versions are the
// objects themselves, so one path serves both.
func (s *Store) EmptyBucket(ctx context.Context, name string) (int, error) {
	deleted := 0
	paginator := s3.NewListObjectVersionsPaginator(s.api, &s3.ListObjectVersionsInput{
		Bucket:  aws.String(name),
		MaxKeys: aws.Int32(objectstore.MaxListLimit),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return deleted, mapErr(err, "list the versions in "+name)
		}
		ids := make([]types.ObjectIdentifier, 0, len(page.Versions)+len(page.DeleteMarkers))
		for _, v := range page.Versions {
			ids = append(ids, types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range page.DeleteMarkers {
			ids = append(ids, types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		if err := s.deleteBatch(ctx, name, ids); err != nil {
			return deleted, err
		}
		deleted += len(ids)
	}
	return deleted, nil
}

// GetVersioning reads the versioning state.
func (s *Store) GetVersioning(ctx context.Context, bucket string) (objectstore.Versioning, error) {
	out, err := s.api.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)})
	if err != nil {
		return "", mapErr(err, "get versioning of "+bucket)
	}
	switch out.Status {
	case types.BucketVersioningStatusEnabled:
		return objectstore.VersioningEnabled, nil
	case types.BucketVersioningStatusSuspended:
		return objectstore.VersioningSuspended, nil
	default:
		return objectstore.VersioningOff, nil
	}
}

// SetVersioning enables or suspends versioning.
func (s *Store) SetVersioning(ctx context.Context, bucket string, enabled bool) error {
	status := types.BucketVersioningStatusSuspended
	if enabled {
		status = types.BucketVersioningStatusEnabled
	}
	_, err := s.api.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String(bucket),
		VersioningConfiguration: &types.VersioningConfiguration{Status: status},
	})
	if err != nil {
		return mapErr(err, "set versioning of "+bucket)
	}
	return nil
}

// corsRuleID marks the one CORS rule Kumo owns on a bucket. SetCORS replaces
// that rule and keeps every other, so an operator's own rules survive.
const corsRuleID = "kumo-console"

// SetCORS lets the console's origins use presigned URLs on the bucket.
func (s *Store) SetCORS(ctx context.Context, bucket string, origins []string) error {
	var rules []types.CORSRule
	current, err := s.api.GetBucketCors(ctx, &s3.GetBucketCorsInput{Bucket: aws.String(bucket)})
	switch {
	case err == nil:
		for _, r := range current.CORSRules {
			if aws.ToString(r.ID) != corsRuleID {
				rules = append(rules, r)
			}
		}
	case isCode(err, "NoSuchCORSConfiguration"):
	default:
		return mapErr(err, "read the CORS rules of "+bucket)
	}

	if len(origins) > 0 {
		rules = append(rules, types.CORSRule{
			ID:             aws.String(corsRuleID),
			AllowedOrigins: origins,
			AllowedMethods: []string{http.MethodGet, http.MethodPut, http.MethodHead},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"ETag"},
			MaxAgeSeconds:  aws.Int32(3600),
		})
	}

	if len(rules) == 0 {
		if _, err := s.api.DeleteBucketCors(ctx, &s3.DeleteBucketCorsInput{Bucket: aws.String(bucket)}); err != nil {
			return mapErr(err, "remove the CORS rules of "+bucket)
		}
		return nil
	}
	_, err = s.api.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket:            aws.String(bucket),
		CORSConfiguration: &types.CORSConfiguration{CORSRules: rules},
	})
	if err != nil {
		return mapErr(err, "set the CORS rules of "+bucket)
	}
	return nil
}

// ListObjects returns one page of a listing.
func (s *Store) ListObjects(ctx context.Context, bucket string, in objectstore.ListInput) (objectstore.ObjectPage, error) {
	limit := in.Limit
	if limit <= 0 || limit > objectstore.MaxListLimit {
		limit = objectstore.MaxListLimit
	}
	req := &s3.ListObjectsV2Input{
		Bucket:  aws.String(bucket),
		MaxKeys: aws.Int32(int32(limit)), //nolint:gosec // bounded by MaxListLimit above
	}
	if in.Prefix != "" {
		req.Prefix = aws.String(in.Prefix)
	}
	if in.Delimiter != "" {
		req.Delimiter = aws.String(in.Delimiter)
	}
	if in.Cursor != "" {
		req.ContinuationToken = aws.String(in.Cursor)
	}

	out, err := s.api.ListObjectsV2(ctx, req)
	if err != nil {
		return objectstore.ObjectPage{}, mapErr(err, "list objects in "+bucket)
	}

	page := objectstore.ObjectPage{
		Objects:        make([]objectstore.Object, 0, len(out.Contents)),
		CommonPrefixes: make([]string, 0, len(out.CommonPrefixes)),
	}
	for _, o := range out.Contents {
		page.Objects = append(page.Objects, objectstore.Object{
			Key:          aws.ToString(o.Key),
			SizeBytes:    aws.ToInt64(o.Size),
			LastModified: aws.ToTime(o.LastModified).UTC(),
			ETag:         strings.Trim(aws.ToString(o.ETag), `"`),
		})
	}
	for _, p := range out.CommonPrefixes {
		page.CommonPrefixes = append(page.CommonPrefixes, aws.ToString(p.Prefix))
	}
	if aws.ToBool(out.IsTruncated) {
		page.NextCursor = aws.ToString(out.NextContinuationToken)
	}
	return page, nil
}

// HeadObject reads one object's metadata.
func (s *Store) HeadObject(ctx context.Context, bucket, key string) (objectstore.Object, error) {
	out, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		return objectstore.Object{}, mapErr(err, "head object "+bucket+"/"+key)
	}
	return objectstore.Object{
		Key:          key,
		SizeBytes:    aws.ToInt64(out.ContentLength),
		LastModified: aws.ToTime(out.LastModified).UTC(),
		ETag:         strings.Trim(aws.ToString(out.ETag), `"`),
		ContentType:  aws.ToString(out.ContentType),
	}, nil
}

// PutObject buffers a small body and writes it. Buffering makes the body
// seekable, which SigV4 needs to hash the payload over plain HTTP.
func (s *Store) PutObject(ctx context.Context, bucket, key string, r io.Reader, size int64, contentType string) error {
	if size < 0 || size > maxPutBytes {
		return fmt.Errorf("%w: put %s/%s: %d bytes is outside 0..%d", ErrPutTooLarge, bucket, key, size, maxPutBytes)
	}
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return fmt.Errorf("rustfs: put %s/%s: read the body: %w", bucket, key, err)
	}
	req := &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(size),
	}
	if contentType != "" {
		req.ContentType = aws.String(contentType)
	}
	if _, err := s.api.PutObject(ctx, req); err != nil {
		return mapErr(err, "put object "+bucket+"/"+key)
	}
	return nil
}

// DeleteObjects removes keys in batches of the S3 maximum, 1000 per request.
func (s *Store) DeleteObjects(ctx context.Context, bucket string, keys []string) error {
	for start := 0; start < len(keys); start += objectstore.MaxListLimit {
		batch := keys[start:min(start+objectstore.MaxListLimit, len(keys))]
		ids := make([]types.ObjectIdentifier, 0, len(batch))
		for _, key := range batch {
			ids = append(ids, types.ObjectIdentifier{Key: aws.String(key)})
		}
		if err := s.deleteBatch(ctx, bucket, ids); err != nil {
			return err
		}
	}
	return nil
}

// deleteBatch sends one DeleteObjects request of at most 1000 entries.
func (s *Store) deleteBatch(ctx context.Context, bucket string, ids []types.ObjectIdentifier) error {
	if len(ids) == 0 {
		return nil
	}
	out, err := s.api.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String(bucket),
		Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return mapErr(err, "delete objects in "+bucket)
	}
	for _, e := range out.Errors {
		if aws.ToString(e.Code) == "NoSuchKey" {
			continue
		}
		return fmt.Errorf("%w: delete %s/%s: %s: %s",
			ErrServer, bucket, aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
	}
	return nil
}

// PresignGet signs a download URL for the public address.
func (s *Store) PresignGet(ctx context.Context, bucket, key string, in objectstore.PresignGetInput) (objectstore.Presigned, error) {
	ttl, err := checkTTL(in.TTL)
	if err != nil {
		return objectstore.Presigned{}, err
	}
	req := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if in.Filename != "" {
		req.ResponseContentDisposition = aws.String(attachment(in.Filename))
	}
	signed, err := s.presign.PresignGetObject(ctx, req, s3.WithPresignExpires(ttl))
	if err != nil {
		return objectstore.Presigned{}, mapErr(err, "presign a download of "+bucket+"/"+key)
	}
	return s.presigned(signed.URL, signed.Method, signed.SignedHeader, ttl), nil
}

// PresignPut signs an upload URL for the public address. The content type is
// part of the signature, so the browser must send the same header.
func (s *Store) PresignPut(ctx context.Context, bucket, key string, in objectstore.PresignPutInput) (objectstore.Presigned, error) {
	ttl, err := checkTTL(in.TTL)
	if err != nil {
		return objectstore.Presigned{}, err
	}
	req := &s3.PutObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if in.ContentType != "" {
		req.ContentType = aws.String(in.ContentType)
	}
	signed, err := s.presign.PresignPutObject(ctx, req, s3.WithPresignExpires(ttl))
	if err != nil {
		return objectstore.Presigned{}, mapErr(err, "presign an upload to "+bucket+"/"+key)
	}
	return s.presigned(signed.URL, signed.Method, signed.SignedHeader, ttl), nil
}

// presigned converts the SDK result. Host is dropped from the headers: a
// browser sets it from the URL and refuses to let a script set it.
func (s *Store) presigned(rawURL, method string, signed http.Header, ttl time.Duration) objectstore.Presigned {
	headers := map[string]string{}
	for name := range maps.Keys(signed) {
		if strings.EqualFold(name, "Host") {
			continue
		}
		headers[name] = signed.Get(name)
	}
	return objectstore.Presigned{
		URL:       rawURL,
		Method:    method,
		Headers:   headers,
		ExpiresAt: s.now().Add(ttl).UTC(),
	}
}

// checkTTL bounds a presigned URL's lifetime to what SigV4 allows.
func checkTTL(ttl time.Duration) (time.Duration, error) {
	if ttl <= 0 || ttl > maxPresignTTL {
		return 0, fmt.Errorf("%w: must be between 1s and %s, got %s", ErrPresignTTL, maxPresignTTL, ttl)
	}
	return ttl, nil
}

// attachment renders a Content-Disposition that saves under filename, with
// the RFC 5987 form for names that are not plain ASCII.
func attachment(filename string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, filename)
	return `attachment; filename="` + ascii + `"; filename*=UTF-8''` + url.PathEscape(filename)
}

// isCode reports whether err is an S3 error with the given code.
func isCode(err error, code string) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == code
}

// mapErr translates SDK errors into the objectstore sentinels, keeping the
// server's code and message for the log.
func mapErr(err error, op string) error {
	var sendErr *smithyhttp.RequestSendError
	if errors.As(err, &sendErr) {
		return fmt.Errorf("%w: %s: %w", objectstore.ErrUnavailable, op, err)
	}

	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return fmt.Errorf("rustfs: %s: %w", op, err)
	}
	switch apiErr.ErrorCode() {
	case "NoSuchBucket", "NoSuchKey", "NotFound":
		return fmt.Errorf("%w: %s: %s", objectstore.ErrNotFound, op, apiErr.ErrorCode())
	case "BucketAlreadyExists", "BucketAlreadyOwnedByYou":
		return fmt.Errorf("%w: %s: %s", objectstore.ErrAlreadyExists, op, apiErr.ErrorCode())
	case "BucketNotEmpty":
		return fmt.Errorf("%w: %s", objectstore.ErrBucketNotEmpty, op)
	case "InvalidBucketName", "KeyTooLongError", "InvalidObjectName":
		return fmt.Errorf("%w: %s: %s", objectstore.ErrInvalidName, op, apiErr.ErrorMessage())
	case "InvalidAccessKeyId", "SignatureDoesNotMatch", "AccessDenied", "ServiceUnavailable", "SlowDown":
		return fmt.Errorf("%w: %s: %s: %s", objectstore.ErrUnavailable, op, apiErr.ErrorCode(), apiErr.ErrorMessage())
	default:
		return fmt.Errorf("%w: %s: %s: %s", ErrServer, op, apiErr.ErrorCode(), apiErr.ErrorMessage())
	}
}
