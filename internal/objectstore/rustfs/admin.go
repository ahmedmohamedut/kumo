package rustfs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
)

// adminPrefix is the MinIO-compatible admin API RustFS serves. It answers
// the same calls under /rustfs/admin/v3; the MinIO path is used because it is
// the one other S3 servers also recognize.
const adminPrefix = "/minio/admin/v3/"

// ErrAdmin is an admin API error with no objectstore equivalent.
var ErrAdmin = errors.New("rustfs: admin API error")

// adminError is the JSON body of an admin API failure.
type adminError struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
}

// policyName is the policy that belongs to one access key.
func policyName(accessKey string) string { return "kumo-" + strings.ToLower(accessKey) }

// policyDocument is the IAM policy a grant becomes: the level's bucket
// actions on each bucket and its object actions on everything inside.
func policyDocument(grant objectstore.Grant) ([]byte, error) {
	if len(grant.Buckets) == 0 {
		return nil, fmt.Errorf("%w: a grant must name at least one bucket", objectstore.ErrInvalidName)
	}
	bucketActions, objectActions := grant.Level.Actions()
	buckets := slices.Sorted(slices.Values(grant.Buckets))
	bucketARNs := make([]string, 0, len(buckets))
	objectARNs := make([]string, 0, len(buckets))
	for _, b := range buckets {
		bucketARNs = append(bucketARNs, "arn:aws:s3:::"+b)
		objectARNs = append(objectARNs, "arn:aws:s3:::"+b+"/*")
	}
	type statement struct {
		Effect   string   `json:"Effect"`
		Action   []string `json:"Action"`
		Resource []string `json:"Resource"`
	}
	return json.Marshal(struct {
		Version   string      `json:"Version"`
		Statement []statement `json:"Statement"`
	}{
		Version: "2012-10-17",
		Statement: []statement{
			{Effect: "Allow", Action: bucketActions, Resource: bucketARNs},
			{Effect: "Allow", Action: objectActions, Resource: objectARNs},
		},
	})
}

// CreateAccessKey makes the policy, then the user, then attaches one to the
// other. A failure removes whatever was made, so the server never keeps a
// user with no policy (which can do nothing) or a policy nobody uses.
func (s *Store) CreateAccessKey(ctx context.Context, spec objectstore.AccessKeySpec) error {
	if spec.AccessKey == "" || spec.SecretKey == "" {
		return fmt.Errorf("%w: an access key needs an id and a secret", objectstore.ErrInvalidName)
	}
	if err := s.putPolicy(ctx, spec.AccessKey, spec.Grant); err != nil {
		return err
	}

	body, err := json.Marshal(map[string]string{"secretKey": spec.SecretKey, "status": "enabled"})
	if err != nil {
		return fmt.Errorf("rustfs: encode the user: %w", err)
	}
	sealed, err := encryptAdmin(s.admin.SecretAccessKey, body)
	if err != nil {
		return err
	}
	if _, err := s.adminCall(ctx, http.MethodPut, "add-user", url.Values{"accessKey": {spec.AccessKey}}, sealed); err != nil {
		s.undo(ctx, spec.AccessKey, false)
		return err
	}

	if _, err := s.adminCall(ctx, http.MethodPut, "set-user-or-group-policy", url.Values{
		"policyName":  {policyName(spec.AccessKey)},
		"userOrGroup": {spec.AccessKey},
		"isGroup":     {"false"},
	}, nil); err != nil {
		s.undo(ctx, spec.AccessKey, true)
		return err
	}
	return nil
}

// SetAccessKeyPolicy rewrites the key's policy in place; the server applies
// it to the key's next request.
func (s *Store) SetAccessKeyPolicy(ctx context.Context, accessKey string, grant objectstore.Grant) error {
	return s.putPolicy(ctx, accessKey, grant)
}

// DeleteAccessKey removes the user first, so the key stops working even if
// the policy removal then fails.
func (s *Store) DeleteAccessKey(ctx context.Context, accessKey string) error {
	if _, err := s.adminCall(ctx, http.MethodDelete, "remove-user", url.Values{"accessKey": {accessKey}}, nil); err != nil &&
		!errors.Is(err, objectstore.ErrNotFound) {
		return err
	}
	if _, err := s.adminCall(ctx, http.MethodDelete, "remove-canned-policy", url.Values{"name": {policyName(accessKey)}}, nil); err != nil &&
		!errors.Is(err, objectstore.ErrNotFound) {
		return err
	}
	return nil
}

// ListAccessKeys reads the user list, which the server encrypts.
func (s *Store) ListAccessKeys(ctx context.Context) ([]string, error) {
	sealed, err := s.adminCall(ctx, http.MethodGet, "list-users", nil, nil)
	if err != nil {
		return nil, err
	}
	plain, err := decryptAdmin(s.admin.SecretAccessKey, sealed)
	if err != nil {
		return nil, err
	}
	var users map[string]json.RawMessage
	if err := json.Unmarshal(plain, &users); err != nil {
		return nil, fmt.Errorf("%w: decode the user list: %w", ErrAdmin, err)
	}
	return slices.Sorted(func(yield func(string) bool) {
		for k := range users {
			if !yield(k) {
				return
			}
		}
	}), nil
}

func (s *Store) putPolicy(ctx context.Context, accessKey string, grant objectstore.Grant) error {
	doc, err := policyDocument(grant)
	if err != nil {
		return err
	}
	_, err = s.adminCall(ctx, http.MethodPut, "add-canned-policy", url.Values{"name": {policyName(accessKey)}}, doc)
	return err
}

// undo removes a half-made access key. Its errors are not returned: the
// caller is already reporting the failure that made the undo necessary.
func (s *Store) undo(ctx context.Context, accessKey string, user bool) {
	ctx = context.WithoutCancel(ctx)
	if user {
		_, _ = s.adminCall(ctx, http.MethodDelete, "remove-user", url.Values{"accessKey": {accessKey}}, nil)
	}
	_, _ = s.adminCall(ctx, http.MethodDelete, "remove-canned-policy", url.Values{"name": {policyName(accessKey)}}, nil)
}

// adminCall signs and sends one admin API request and returns the body of a
// 2xx answer.
func (s *Store) adminCall(ctx context.Context, method, call string, query url.Values, body []byte) ([]byte, error) {
	target := s.endpoint + adminPrefix + call
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("rustfs: admin %s: %w", call, err)
	}
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Content-Sha256", payload)
	if err := v4.NewSigner().SignHTTP(ctx, s.admin, req, payload, "s3", s.region, s.now()); err != nil {
		return nil, fmt.Errorf("rustfs: sign admin %s: %w", call, err)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: admin %s: %w", objectstore.ErrUnavailable, call, err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("%w: admin %s: %w", objectstore.ErrUnavailable, call, err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return data, nil
	}

	var failure adminError
	_ = json.Unmarshal(data, &failure)
	switch {
	case resp.StatusCode == http.StatusNotFound,
		strings.Contains(failure.Code, "NoSuchUser"), strings.Contains(failure.Code, "NoSuchPolicy"):
		return nil, fmt.Errorf("%w: admin %s: %s", objectstore.ErrNotFound, call, failure.Code)
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusServiceUnavailable:
		return nil, fmt.Errorf("%w: admin %s: %d %s: %s", objectstore.ErrUnavailable, call,
			resp.StatusCode, failure.Code, failure.Message)
	default:
		return nil, fmt.Errorf("%w: %s: %d %s: %s", ErrAdmin, call, resp.StatusCode, failure.Code, failure.Message)
	}
}
