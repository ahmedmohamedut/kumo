package image

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/ahmedmohamedut/kumo/internal/service"
)

// Bounds of an import request, from docs/design/01-api.md §Images and the CHECK
// constraints of docs/design/02-data-model.md.
const (
	// maxNameLength and maxDescriptionLength mirror the key pair limits, so the
	// console can render both without special cases.
	maxNameLength        = 64
	maxDescriptionLength = 256

	// MinDiskGB is the smallest root disk a cloud image boots from.
	MinDiskGB = 1
	// MaxDiskGB bounds a requested root disk. Image import does not create the
	// root disk (M4.3 does), but the value is validated now so the console gets
	// an inline error instead of a boot failure later.
	MaxDiskGB = 65536 // 64 TiB

	// MaxOSVersionLength bounds os_version; the column is a VARCHAR(64).
	MaxOSVersionLength = 64
	// MaxDefaultUserLength bounds default_user.
	MaxDefaultUserLength = 64

	// maxURLLength bounds source_url so an absurd URL cannot fill the row.
	maxURLLength = 2048

	// maxTags, maxTagKey and maxTagValue mirror the tag limits of the key pair
	// service; the table is shared.
	maxTags     = 50
	maxTagKey   = 64
	maxTagValue = 256

	// Checksum hex lengths of the two supported algorithms.
	sha256HexLength = 64
	sha512HexLength = 128
)

// namePattern is the image name shape: letters, digits, dot, underscore and
// dash, because the name is also used as a volume file name.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// validateImport checks every field of a POST /images/import body. All problems
// are returned at once, so the console can mark every bad input in one pass.
func (s *Service) validateImport(in ImportInput) []service.FieldError {
	var fields []service.FieldError

	if !namePattern.MatchString(in.Name) {
		fields = append(fields, service.FieldError{
			Field:  "name",
			Reason: fmt.Sprintf("must be 1-%d characters of letters, digits, dot, underscore or dash", maxNameLength),
		})
	}

	if len(in.Description) > maxDescriptionLength {
		fields = append(fields, service.FieldError{
			Field:  "description",
			Reason: fmt.Sprintf("must be at most %d characters", maxDescriptionLength),
		})
	}

	if err := validateSourceURL(in.SourceURL); err != nil {
		fields = append(fields, *err)
	} else if err := s.downloader.Validate(in.SourceURL); err != nil {
		// The URL parses and looks harmless, but it points somewhere Kumo must
		// not fetch on an operator's behalf. Saying so now beats a task that
		// fails five minutes later.
		fields = append(fields, service.FieldError{
			Field:  "source_url",
			Reason: "points at an address image downloads are not allowed to use",
		})
	}

	if err := validateChecksum(in.Checksum); err != nil {
		fields = append(fields, *err)
	}

	if in.Architecture != "" && in.Architecture != ArchX86_64 && in.Architecture != ArchAarch64 {
		fields = append(fields, service.FieldError{
			Field:  "architecture",
			Reason: `must be "x86_64" or "aarch64"`,
		})
	}

	if len(in.OSFamily) > MaxOSVersionLength {
		fields = append(fields, service.FieldError{
			Field: "os_family", Reason: fmt.Sprintf("must be at most %d characters", MaxOSVersionLength),
		})
	}
	if len(in.OSVersion) > MaxOSVersionLength {
		fields = append(fields, service.FieldError{
			Field: "os_version", Reason: fmt.Sprintf("must be at most %d characters", MaxOSVersionLength),
		})
	}
	if len(in.DefaultUser) > MaxDefaultUserLength {
		fields = append(fields, service.FieldError{
			Field:  "default_user",
			Reason: fmt.Sprintf("must be at most %d characters", MaxDefaultUserLength),
		})
	}

	if in.MinDiskGB != 0 && (in.MinDiskGB < MinDiskGB || in.MinDiskGB > MaxDiskGB) {
		fields = append(fields, service.FieldError{
			Field:  "min_disk_gb",
			Reason: fmt.Sprintf("must be between %d and %d GiB", MinDiskGB, MaxDiskGB),
		})
	}

	if field := validateTags(in.Tags); field != nil {
		fields = append(fields, *field)
	}

	return fields
}

// validateSourceURL checks the URL syntactically. Whether it is reachable, and
// whether it points somewhere it must not, is decided by the downloader when the
// worker runs (internal/imagefetch), which re-validates every redirect hop.
func validateSourceURL(raw string) *service.FieldError {
	if strings.TrimSpace(raw) == "" {
		return &service.FieldError{Field: "source_url", Reason: "is required"}
	}
	if len(raw) > maxURLLength {
		return &service.FieldError{
			Field: "source_url", Reason: fmt.Sprintf("must be at most %d characters", maxURLLength),
		}
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return &service.FieldError{Field: "source_url", Reason: "is not a valid URL"}
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return &service.FieldError{Field: "source_url", Reason: `must be an http or https URL`}
	}
	if parsed.Host == "" {
		return &service.FieldError{Field: "source_url", Reason: "must include a host"}
	}
	if parsed.User != nil {
		return &service.FieldError{Field: "source_url", Reason: "must not carry credentials"}
	}
	if parsed.RawQuery != "" {
		return &service.FieldError{
			Field: "source_url", Reason: "must not carry a query string, so the URL stays reproducible",
		}
	}

	return nil
}

// validateChecksum requires the algorithm and the value together, and checks the
// value is the right hex length for the algorithm.
func validateChecksum(checksum Checksum) *service.FieldError {
	if checksum.IsZero() {
		return nil
	}
	if checksum.Algorithm == "" || checksum.Value == "" {
		return &service.FieldError{
			Field: "checksum", Reason: "algorithm and value must be given together",
		}
	}

	var want int
	switch checksum.Algorithm {
	case ChecksumSHA256:
		want = sha256HexLength
	case ChecksumSHA512:
		want = sha512HexLength
	default:
		return &service.FieldError{Field: "checksum.algorithm", Reason: `must be "sha256" or "sha512"`}
	}

	if !isHex(checksum.Value) {
		return &service.FieldError{Field: "checksum.value", Reason: "must be hexadecimal"}
	}
	if len(checksum.Value) != want {
		return &service.FieldError{
			Field:  "checksum.value",
			Reason: fmt.Sprintf("must be %d hexadecimal characters for %s", want, checksum.Algorithm),
		}
	}

	return nil
}

// validateUpdate checks a PATCH body. A nil field means "leave it alone".
func validateUpdate(in UpdateInput) []service.FieldError {
	var fields []service.FieldError

	if in.Name != nil && !namePattern.MatchString(*in.Name) {
		fields = append(fields, service.FieldError{
			Field:  "name",
			Reason: fmt.Sprintf("must be 1-%d characters of letters, digits, dot, underscore or dash", maxNameLength),
		})
	}
	if in.Description != nil && len(*in.Description) > maxDescriptionLength {
		fields = append(fields, service.FieldError{
			Field:  "description",
			Reason: fmt.Sprintf("must be at most %d characters", maxDescriptionLength),
		})
	}
	if field := validateTags(in.Tags); field != nil {
		fields = append(fields, *field)
	}

	return fields
}

// validateTags enforces the tag limits of docs/design/02-data-model.md.
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

// isHex reports whether every character of value is a hex digit.
func isHex(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}
