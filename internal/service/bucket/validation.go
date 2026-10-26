package bucket

import (
	"fmt"
	"net/netip"
	"regexp"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/service"
)

// Tag limits of 02-data-model.md, the same for every resource.
const (
	maxTags     = 50
	maxTagKey   = 128
	maxTagValue = 256
)

// namePattern is the character rule of an S3 bucket name: 3-63 lowercase
// letters, digits, '.' and '-', starting and ending with a letter or digit.
var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// nameReason is the one message for every refused name, so a form shows the
// whole rule rather than the first clause that failed.
const nameReason = "must be 3-63 lowercase letters, digits, '.' or '-', start and end with a letter or digit, " +
	"have no '..', and not look like an IP address"

// validName applies the S3 naming rules, including the reserved prefixes and
// suffixes AWS documents. RustFS enforces most of them too; checking here
// gives a field error instead of a server message.
func validName(name string) bool {
	if !namePattern.MatchString(name) || strings.Contains(name, "..") {
		return false
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return false
	}
	for _, prefix := range []string{"xn--", "sthree-", "amzn-s3-demo-"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	for _, suffix := range []string{"-s3alias", "--ol-s3", "--x-s3", ".mrap"} {
		if strings.HasSuffix(name, suffix) {
			return false
		}
	}
	return true
}

func validateName(name string) *service.FieldError {
	if name == "" {
		return &service.FieldError{Field: "name", Reason: "must not be empty"}
	}
	if !validName(name) {
		return &service.FieldError{Field: "name", Reason: nameReason}
	}
	return nil
}

// validateTags enforces the tag limits of 02-data-model.md.
func validateTags(tags map[string]string) *service.FieldError {
	if len(tags) > maxTags {
		return &service.FieldError{Field: "tags", Reason: fmt.Sprintf("at most %d tags per resource", maxTags)}
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
