//go:build rustfs

package rustfs

import (
	"context"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// PutRawCORS writes a CORS configuration of one rule that is not Kumo's, the
// way an operator would with another client.
func PutRawCORS(ctx context.Context, s *Store, bucket, id, origin string) error {
	_, err := s.api.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: aws.String(bucket),
		CORSConfiguration: &types.CORSConfiguration{CORSRules: []types.CORSRule{{
			ID:             aws.String(id),
			AllowedOrigins: []string{origin},
			AllowedMethods: []string{http.MethodPut},
		}}},
	})
	return err
}
