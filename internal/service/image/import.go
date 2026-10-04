package image

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/imagefmt"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

// progressStep is how many percent points must pass before the row is written
// again. Writing every chunk would turn a 20 GiB download into millions of
// UPDATEs, which SQLite serializes.
const progressStep = 5

// ErrChecksumMismatch means the download did not hash to the expected value.
// The image is left failed and the partial file is gone; a retry starts over.
var ErrChecksumMismatch = errors.New("image: the image does not match its checksum")

// ErrBadTaskPayload means a task did not name the image it works on.
var ErrBadTaskPayload = errors.New("image: the task payload is not acceptable")

// ImportHandler downloads an image and registers it as a volume. It runs in the
// worker, never in an HTTP handler (AGENTS.md architecture rule 4).
//
// The steps and their failure behavior, from docs/design/04-libvirt-integration.md
// §2:
//
//  1. mark the row downloading, so the console stops showing "queued";
//  2. download into a temp file, hashing as it goes, refusing an oversized body;
//  3. compare the checksum if one was requested, before anything is published;
//  4. parse the header, which decides qcow2 vs raw and the capacity;
//  5. hand the file to the hypervisor, which places it in the images pool;
//  6. finalize the row.
//
// Any failure before step 6 removes both the temp file and the volume, so a
// retry cannot find a half-imported image or a leftover file filling the pool.
func (s *Service) ImportHandler(ctx context.Context, t worker.Task) error {
	var payload importPayload
	if err := t.Decode(&payload); err != nil {
		return err
	}
	if payload.ImageID == "" {
		return fmt.Errorf("%w: import task %s names no image", ErrBadTaskPayload, t.ID)
	}

	row, err := s.store.GetImage(ctx, sqlc.GetImageParams{ID: payload.ImageID})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The row is gone, so there is nothing to import and nothing to
			// clean up: the image was deleted while the task was queued.
			s.logger.InfoContext(ctx, "image: import skipped, the image is gone",
				"image_id", payload.ImageID, "task_id", t.ID)
			return nil
		}
		return fmt.Errorf("get image %q: %w", payload.ImageID, err)
	}

	// An import that already finished is a no-op. Without this a retried task
	// would download the image a second time.
	if State(row.State) == StateAvailable {
		s.logger.InfoContext(ctx, "image: import already available",
			"image_id", row.ID, "task_id", t.ID)
		return nil
	}

	name := volumeName(row.ID)

	if err := s.setProgress(ctx, row.ID, StateDownloading, "", 0); err != nil {
		return err
	}

	path, err := s.stage(ctx, row)
	if err != nil {
		s.failImport(ctx, row.ID, t.ID, err)
		return err
	}

	// The volume is only created in step 5; from here on a failure must remove
	// it, because a half-written volume in the pool is invisible to the row.
	published := false
	defer func() {
		if !published {
			s.discard(ctx, row.ID, name, path)
		}
	}()

	info, err := imagefmt.Inspect(path)
	if err != nil {
		s.failImport(ctx, row.ID, t.ID, err)
		return fmt.Errorf("inspect the image %q: %w", row.ID, err)
	}

	file, err := os.Open(path) //nolint:gosec // the path is inside our own temp dir
	if err != nil {
		s.failImport(ctx, row.ID, t.ID, err)
		return fmt.Errorf("open the staged image %q: %w", row.ID, err)
	}
	defer file.Close() //nolint:errcheck // read-only handle

	if err := s.hv.ImportImage(ctx, name, file, hypervisor.ImportSpec{
		SizeBytes:        info.SizeBytes,
		VirtualSizeBytes: info.VirtualSizeBytes,
		Format:           hypervisor.ImageFormat(info.Format),
	}); err != nil {
		s.failImport(ctx, row.ID, t.ID, err)
		return fmt.Errorf("register image %q as a volume: %w", row.ID, err)
	}
	// The volume exists now: from here the defer must not delete it.
	published = true

	if _, err := s.store.FinalizeImage(ctx, sqlc.FinalizeImageParams{
		ID:               row.ID,
		Format:           string(info.Format),
		SizeBytes:        sql.NullInt64{Int64: info.SizeBytes, Valid: true},
		VirtualSizeBytes: sql.NullInt64{Int64: info.VirtualSizeBytes, Valid: true},
		VolumeName:       sql.NullString{String: name, Valid: true},
		UpdatedAt:        formatTime(s.now()),
	}); err != nil {
		// The volume is good but the row is not. Remove the volume so the next
		// retry starts clean; the row goes back to pending.
		published = false
		s.failImport(ctx, row.ID, t.ID, err)
		return fmt.Errorf("finalize image %q: %w", row.ID, err)
	}

	_ = os.Remove(path) // the file is now the pool volume, or was renamed there

	s.logger.InfoContext(ctx, "image: import finished",
		"image_id", row.ID, "format", string(info.Format),
		"size_bytes", info.SizeBytes, "virtual_size_bytes", info.VirtualSizeBytes)

	return nil
}

// stage downloads the image into a temp file and checks it. It returns the path
// of the finished file; the caller removes it once it has become a volume.
func (s *Service) stage(ctx context.Context, row sqlc.Image) (string, error) {
	if !row.SourceUrl.Valid || row.SourceUrl.String == "" {
		return "", fmt.Errorf("%w: image %q has no source URL", service.ErrConflict, row.ID)
	}

	if err := os.MkdirAll(s.tempDir, 0o700); err != nil {
		return "", fmt.Errorf("create the staging directory %s: %w", s.tempDir, err)
	}
	file, err := os.CreateTemp(s.tempDir, "kumo-image-*.img")
	if err != nil {
		return "", fmt.Errorf("create a staging file in %s: %w", s.tempDir, err)
	}
	path := file.Name()
	// The file is removed on every error path below, and by the caller once it
	// has been handed to the hypervisor.
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(path)
		}
	}()

	digest, err := newDigest(row)
	if err != nil {
		return "", err
	}

	if err := s.download(ctx, row, file, digest); err != nil {
		return "", err
	}

	if err := file.Sync(); err != nil {
		return "", fmt.Errorf("flush the staged image %q: %w", row.ID, err)
	}

	if row.ChecksumAlgo.Valid && row.ChecksumValue.Valid {
		// Checked before the volume is created: a wrong checksum must not reach
		// the pool, where it would look launchable.
		if err := compareDigest(digest, row.ChecksumAlgo.String, row.ChecksumValue.String); err != nil {
			return "", err
		}
	}

	keep = true
	return path, nil
}

// download streams the URL into the file, hashing as it goes and reporting
// progress in whole percents of Content-Length.
func (s *Service) download(
	ctx context.Context, row sqlc.Image, file *os.File, digest hash.Hash,
) error {
	fetch, err := s.downloader.Fetch(ctx, row.SourceUrl.String)
	if err != nil {
		return fmt.Errorf("fetch image %q: %w", row.ID, err)
	}
	defer fetch.Body.Close() //nolint:errcheck // the download result is what matters

	// Progress is a percentage of Content-Length, which means a chunked
	// response cannot report any: progress stays 0 until the image is published
	// and finalize writes 100. A percentage of the size limit instead would be a
	// number nobody can act on (600 MB against a 20 GiB limit is 3% for the whole
	// download), so the honest 0 stands and the state carries the meaning.
	// docs/design/01-api.md §Images documents it.
	total := fetch.ContentLength

	var written int64
	reporter := &progressReporter{
		ctx:        ctx,
		svc:        s,
		imageID:    row.ID,
		total:      total,
		last:       -progressStep,
		downloaded: &written,
	}
	// digest is nil when the image carries no checksum; io.MultiWriter would
	// panic on a nil writer.
	sinks := []io.Writer{file, reporter}
	if digest != nil {
		sinks = append([]io.Writer{digest}, sinks...)
	}
	writer := io.MultiWriter(sinks...)

	if _, err := io.Copy(writer, fetch.Body); err != nil {
		// The downloader returns ErrTooLarge here; keep it recognizable.
		return fmt.Errorf("download image %q: %w", row.ID, err)
	}

	// The truncation check uses the promised length only; the fallback total is
	// not a promise the server made.
	if fetch.ContentLength > 0 && written != fetch.ContentLength {
		return fmt.Errorf("%w: image %q is %d bytes, the server promised %d",
			imagefmt.ErrTruncated, row.ID, written, fetch.ContentLength)
	}
	if written == 0 {
		return fmt.Errorf("%w: image %q is empty", imagefmt.ErrTruncated, row.ID)
	}

	// The last percent is written even when the stream ended below a step
	// boundary, so the console never sticks at 97%.
	return s.setProgress(ctx, row.ID, StateDownloading, "", 100)
}

// setProgress writes state and progress together, because the download is the
// only thing that moves an image out of pending.
func (s *Service) setProgress(
	ctx context.Context, imageID string, state State, reason string, progress int,
) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}

	if _, err := s.store.UpdateImageProgress(ctx, sqlc.UpdateImageProgressParams{
		ID:          imageID,
		State:       string(state),
		StateReason: nullableString(reason),
		Progress:    int64(progress),
		UpdatedAt:   formatTime(s.now()),
	}); err != nil {
		return fmt.Errorf("record the progress of image %q: %w", imageID, err)
	}
	return nil
}

// failImport marks the row failed with the reason. A failure to write the reason
// is logged and not returned: the caller already returns the original error, and
// losing the message must not hide the cause.
func (s *Service) failImport(ctx context.Context, imageID, taskID string, cause error) {
	reason := cause.Error()

	if _, err := s.store.FailImage(ctx, sqlc.FailImageParams{
		ID:          imageID,
		StateReason: sql.NullString{String: reason, Valid: true},
		UpdatedAt:   formatTime(s.now()),
	}); err != nil {
		s.logger.ErrorContext(ctx, "image: could not record the import failure",
			"image_id", imageID, "reason", reason, "error", err)
		return
	}

	s.logger.WarnContext(ctx, "image: import failed",
		"image_id", imageID, "task_id", taskID, "reason", reason)
}

// discard removes what a failed import left behind: the temp file always, and the
// volume only if it may exist. It is best effort and only logged, because it runs
// on the error path where the original error is what the caller must see.
func (s *Service) discard(ctx context.Context, imageID, volume, path string) {
	if path != "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.logger.ErrorContext(ctx, "image: could not remove the staged file",
				"image_id", imageID, "path", path, "error", err)
		}
	}

	if err := s.hv.DeleteVolume(ctx, s.imagesPool, volume); err != nil &&
		!errors.Is(err, hypervisor.ErrNotFound) {
		s.logger.ErrorContext(ctx, "image: could not remove the partial volume",
			"image_id", imageID, "volume", volume, "error", err)
	}
}

// newDigest returns the hash the checksum is compared against, or nil when the
// image carries no checksum.
func newDigest(row sqlc.Image) (hash.Hash, error) {
	switch ChecksumAlgorithm(row.ChecksumAlgo.String) {
	case "":
		return nil, nil
	case ChecksumSHA256:
		return sha256.New(), nil
	case ChecksumSHA512:
		return sha512.New(), nil
	default:
		return nil, fmt.Errorf("%w: image %q asks for checksum %q", service.ErrConflict,
			row.ID, row.ChecksumAlgo.String)
	}
}

// compareDigest checks the running hash against the expected value. The
// comparison is constant time so a mismatch does not leak the hash by timing.
func compareDigest(digest hash.Hash, algorithm, expected string) error {
	if digest == nil {
		return fmt.Errorf("%w: image asks for a %s checksum but no hash was computed",
			ErrChecksumMismatch, algorithm)
	}

	got := hex.EncodeToString(digest.Sum(nil))
	if !equalHex(got, expected) {
		return fmt.Errorf("%w: got %s %s, expected %s",
			ErrChecksumMismatch, algorithm, got, expected)
	}
	return nil
}

// equalHex compares two hex strings without an early exit on the first
// difference. It normalizes the case first, so an uppercase checksum is accepted.
func equalHex(a, b string) bool {
	got := normalizeHex(a)
	want := normalizeHex(b)
	if len(got) != len(want) {
		return false
	}

	var diff byte
	for i := range len(got) {
		diff |= got[i] ^ want[i]
	}
	return diff == 0
}

func normalizeHex(value string) string {
	out := []byte(value)
	for i, c := range out {
		if c >= 'A' && c <= 'F' {
			out[i] = c + ('a' - 'A')
		}
	}
	return string(out)
}

// progressReporter writes image progress while the download streams. It is an
// io.Writer that passes every byte through unchanged.
type progressReporter struct {
	ctx        context.Context
	svc        *Service
	imageID    string
	total      int64
	last       int
	downloaded *int64
}

// Write passes the bytes through and records progress every progressStep percents.
func (p *progressReporter) Write(chunk []byte) (int, error) {
	n := len(chunk)
	*p.downloaded += int64(n)

	if p.total <= 0 {
		return n, nil
	}

	// The download can overshoot the fallback total when the response has no
	// Content-Length, so the cap matters: progress is never 100 before
	// finalize.
	percent := min(int(*p.downloaded*100/p.total), 99)
	if percent < 100 && percent-p.last < progressStep {
		return n, nil
	}
	p.last = percent

	if err := p.svc.setProgress(p.ctx, p.imageID, StateDownloading, "", percent); err != nil {
		// Progress is advisory: a failed UPDATE must not abort the download.
		p.svc.logger.WarnContext(p.ctx, "image: could not record progress",
			"image_id", p.imageID, "percent", percent, "error", err)
	}
	return n, nil
}
