package image_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
	"github.com/ahmedmohamedut/kumo/internal/imagefmt/qcow2test"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/sqlc"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
	"github.com/ahmedmohamedut/kumo/internal/worker"
)

const (
	imagesPool    = "kumo-images"
	instancesPool = "kumo-instances"

	// testInstanceType is the instance_types row the in-use test references.
	testInstanceType = "kumo.small"
)

// virtualDiskGB is the virtual size of the fixture image, 1 GiB.
const virtualDiskGB = 1

var testClock = func() time.Time {
	return time.Date(2026, 3, 4, 9, 0, 0, 0, time.UTC)
}

// harness is the service under test with its store, hypervisor and staging
// directory, for assertions below the API surface.
type harness struct {
	svc    *image.Service
	store  *store.Store
	hv     *fake.Fake
	temp   string
	now    *time.Time
	tasks  *task.Service
	server *httptest.Server
}

func newHarness(t *testing.T) harness {
	t.Helper()

	db := storetest.New(t)
	hv := fake.New(fake.Config{})
	ctx := t.Context()
	if err := hv.EnsureInfra(ctx, hypervisor.InfraConfig{
		ImagesPool:    imagesPool,
		InstancesPool: instancesPool,
		Network:       "kumo-net",
		DataDir:       t.TempDir(),
	}); err != nil {
		t.Fatalf("EnsureInfra(): %v", err)
	}

	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}

	now := testClock()
	temp := t.TempDir()
	svc, err := image.New(image.Options{
		Logger:     logging.Discard(),
		Store:      db,
		Hypervisor: hv,
		Tasks:      tasks,
		// httptest listens on loopback, which the default policy refuses, and a
		// limit is what lets a chunked response through at all.
		Downloader: imagefetch.New(imagefetch.Options{
			AllowPrivateNetworks: true,
			MaxBytes:             4 << 20,
		}),
		ImagesPool: imagesPool,
		TempDir:    temp,
		MaxBytes:   4 << 20,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("image.New(): %v", err)
	}

	return harness{svc: svc, store: db, hv: hv, temp: temp, now: &now, tasks: tasks}
}

// advance steps the harness clock forward by one second. A frozen clock would
// collapse every created_at into a tie, and keyset pagination then orders by id.
func (h harness) advance() { *h.now = h.now.Add(time.Second) }

// serveImage publishes body at /image and returns the URL plus its SHA-256.
func (h *harness) serveImage(t *testing.T, body []byte) (url, sum string) {
	t.Helper()

	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(h.server.Close)

	digest := sha256.Sum256(body)
	return h.server.URL + "/image", hex.EncodeToString(digest[:])
}

// runImport executes the import task of an image the way the worker does.
func (h *harness) runImport(t *testing.T, imageID string) error {
	t.Helper()
	return h.svc.ImportHandler(t.Context(), worker.Task{
		ID:         "task-test",
		Type:       task.TypeImageImport,
		ResourceID: imageID,
		Payload:    []byte(fmt.Sprintf(`{"image_id":%q}`, imageID)),
	})
}

// runDelete executes the delete task of an image the way the worker does.
func (h *harness) runDelete(t *testing.T, imageID string) error {
	t.Helper()
	return h.svc.DeleteHandler(t.Context(), worker.Task{
		ID:         "task-test",
		Type:       task.TypeImageDelete,
		ResourceID: imageID,
		Payload:    []byte(fmt.Sprintf(`{"image_id":%q}`, imageID)),
	})
}

// importTiny queues and runs an import of the tiny qcow2 fixture over httptest,
// which is the M3.2 acceptance path.
func (h *harness) importTiny(t *testing.T, name string) image.Image {
	t.Helper()

	body := qcow2test.Tiny(virtualDiskGB << 30)
	url, sum := h.serveImage(t, body)

	imported, err := h.svc.Import(t.Context(), image.ImportInput{
		Name:      name,
		SourceURL: url,
		Checksum:  image.Checksum{Algorithm: image.ChecksumSHA256, Value: sum},
		Tags:      map[string]string{"env": "test"},
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}
	if err := h.runImport(t, imported.ID); err != nil {
		t.Fatalf("ImportHandler(): %v", err)
	}

	got, err := h.svc.Get(t.Context(), imported.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	return got
}

func TestNewValidatesItsDependencies(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	hv := fake.New(fake.Config{})
	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}
	dl := imagefetch.New(imagefetch.Options{})

	tests := []struct {
		name string
		opts image.Options
		want error
	}{
		{
			name: "no logger",
			opts: image.Options{Store: db, Hypervisor: hv, Tasks: tasks, Downloader: dl,
				ImagesPool: imagesPool, TempDir: t.TempDir()},
			want: image.ErrNoLogger,
		},
		{
			name: "no store",
			opts: image.Options{Logger: logging.Discard(), Hypervisor: hv, Tasks: tasks,
				Downloader: dl, ImagesPool: imagesPool, TempDir: t.TempDir()},
			want: image.ErrNoStore,
		},
		{
			name: "no hypervisor",
			opts: image.Options{Logger: logging.Discard(), Store: db, Tasks: tasks,
				Downloader: dl, ImagesPool: imagesPool, TempDir: t.TempDir()},
			want: image.ErrNoHypervisor,
		},
		{
			name: "no task service",
			opts: image.Options{Logger: logging.Discard(), Store: db, Hypervisor: hv,
				Downloader: dl, ImagesPool: imagesPool, TempDir: t.TempDir()},
			want: image.ErrNoTasks,
		},
		{
			name: "no downloader",
			opts: image.Options{Logger: logging.Discard(), Store: db, Hypervisor: hv,
				Tasks: tasks, ImagesPool: imagesPool, TempDir: t.TempDir()},
			want: image.ErrNoDownloader,
		},
		{
			name: "no pool",
			opts: image.Options{Logger: logging.Discard(), Store: db, Hypervisor: hv,
				Tasks: tasks, Downloader: dl, TempDir: t.TempDir()},
			want: image.ErrNoPool,
		},
		{
			name: "no temp dir",
			opts: image.Options{Logger: logging.Discard(), Store: db, Hypervisor: hv,
				Tasks: tasks, Downloader: dl, ImagesPool: imagesPool},
			want: image.ErrNoTempDir,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := image.New(tt.opts); !errors.Is(err, tt.want) {
				t.Errorf("New() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestImportQueuesATaskAndAnswers(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	url, sum := h.serveImage(t, qcow2test.Tiny(virtualDiskGB<<30))

	imported, err := h.svc.Import(t.Context(), image.ImportInput{
		Name:         "ubuntu-2404",
		Description:  "the LTS image",
		SourceURL:    url,
		Checksum:     image.Checksum{Algorithm: image.ChecksumSHA256, Value: sum},
		OSFamily:     "linux",
		OSVersion:    "24.04",
		Architecture: image.ArchX86_64,
		DefaultUser:  "ubuntu",
		MinDiskGB:    8,
		Tags:         map[string]string{"env": "prod"},
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}

	if !strings.HasPrefix(imported.ID, "img-") || len(imported.ID) != len("img-")+17 {
		t.Errorf("ID = %q, want img- plus 17 characters", imported.ID)
	}
	if !strings.HasPrefix(imported.TaskID, "task-") {
		t.Errorf("TaskID = %q, want a task- prefixed id", imported.TaskID)
	}

	// The response is the image in its queued state, with no size or format
	// yet: the header has not been read.
	if imported.State != image.StatePending {
		t.Errorf("State = %q, want pending", imported.State)
	}
	if imported.Progress != 0 {
		t.Errorf("Progress = %d, want 0", imported.Progress)
	}
	if imported.Format != "" {
		t.Errorf("Format = %q, want empty while pending", imported.Format)
	}
	if imported.SizeBytes != nil {
		t.Errorf("SizeBytes = %v, want null while pending", *imported.SizeBytes)
	}
	if imported.SourceURL == nil || *imported.SourceURL != url {
		t.Errorf("SourceURL = %v, want %q", imported.SourceURL, url)
	}
	if imported.Checksum == nil || imported.Checksum.Algorithm != image.ChecksumSHA256 {
		t.Errorf("Checksum = %+v, want sha256", imported.Checksum)
	}
	if imported.Tags["env"] != "prod" {
		t.Errorf("Tags = %v, want env=prod", imported.Tags)
	}

	// The row and the task must exist together, or a queue could claim an
	// image that was never written.
	queued, err := h.tasks.Get(t.Context(), imported.TaskID)
	if err != nil {
		t.Fatalf("tasks.Get(): %v", err)
	}
	if queued.Type != task.TypeImageImport {
		t.Errorf("task Type = %q, want %q", queued.Type, task.TypeImageImport)
	}
	if queued.State != task.StatePending {
		t.Errorf("task State = %q, want pending", queued.State)
	}
	if queued.ResourceID == nil || *queued.ResourceID != imported.ID {
		t.Errorf("task ResourceID = %v, want %q", queued.ResourceID, imported.ID)
	}
}

func TestImportHandlerPublishesTheImage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	got := h.importTiny(t, "ubuntu-2404")

	if got.State != image.StateAvailable {
		t.Errorf("State = %q, want available (reason %v)", got.State, got.StateReason)
	}
	if got.Progress != 100 {
		t.Errorf("Progress = %d, want 100", got.Progress)
	}
	if string(got.Format) != string(hypervisor.FormatQCOW2) {
		t.Errorf("Format = %q, want qcow2", got.Format)
	}
	if got.SizeBytes == nil || *got.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %v, want the file size", got.SizeBytes)
	}
	if got.StateReason != nil {
		t.Errorf("StateReason = %v, want null once available", *got.StateReason)
	}

	// The volume must exist in the images pool under a name derived from the
	// image id, and carry the fixture bytes.
	info, err := h.hv.ImageInfo(t.Context(), got.ID)
	if err != nil {
		t.Fatalf("ImageInfo(): %v", err)
	}
	if info.Pool != imagesPool {
		t.Errorf("Pool = %q, want %q", info.Pool, imagesPool)
	}
	if info.Format != hypervisor.FormatQCOW2 {
		t.Errorf("volume Format = %q, want qcow2", info.Format)
	}
	if info.VirtualSizeBytes != virtualDiskGB<<30 {
		t.Errorf("virtual size = %d, want %d", info.VirtualSizeBytes, int64(virtualDiskGB)<<30)
	}

	// The staging directory must be empty: the file became the volume, so a
	// leftover copy would be a leak the pool never reclaims.
	assertEmptyDir(t, h.temp)
}

// The image CDNs answer chunked, with no Content-Length. Kumo does not invent a
// percentage from the size limit, so progress stays 0 while the bytes stream and
// only becomes meaningful once the image is published; the state is what tells
// the console something is happening (docs/design/01-api.md §Images).
func TestImportHandlerHandlesAResponseWithoutAContentLength(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	body := qcow2test.Tiny(virtualDiskGB << 30)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		// Flushing before the body is finished forces a chunked response: Go's
		// server only sets Content-Length when the whole body fits in one write.
		_, _ = w.Write(body[:512])
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("the test server does not support flushing")
			return
		}
		flusher.Flush()
		<-release
		_, _ = w.Write(body[512:])
	}))
	defer server.Close()

	img, err := h.svc.Import(t.Context(), image.ImportInput{
		Name: "ubuntu-2404", SourceURL: server.URL + "/noble.img",
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}

	// The handler runs in a goroutine because it blocks on the stalled response;
	// testing.Fatal from a goroutine is not allowed, so the error comes back
	// through a channel.
	done := make(chan error, 1)
	go func() {
		done <- h.svc.ImportHandler(t.Context(), worker.Task{
			ID: "task-test", Type: task.TypeImageImport, Payload: []byte(`{"image_id":"` + img.ID + `"}`),
		})
	}()

	var stalled image.Image
	for range 200 {
		got, err := h.svc.Get(t.Context(), img.ID)
		if err != nil {
			t.Fatalf("Get(): %v", err)
		}
		if got.State == image.StateDownloading {
			stalled = got
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if stalled.Progress != 0 {
		t.Errorf("Progress = %d, want 0: there is no total to be a percentage of",
			stalled.Progress)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("ImportHandler(): %v", err)
	}

	got, err := h.svc.Get(t.Context(), img.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.State != image.StateAvailable {
		t.Fatalf("State = %q (reason %v), want available", got.State, got.StateReason)
	}
	if got.Progress != 100 {
		t.Errorf("Progress = %d, want 100 once available", got.Progress)
	}
	if got.SizeBytes == nil || *got.SizeBytes != int64(len(body)) {
		t.Errorf("SizeBytes = %v, want %d: a chunked download still records the length",
			got.SizeBytes, len(body))
	}
	assertEmptyDir(t, h.temp)
}

func TestImportHandlerIsIdempotent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	imported := h.importTiny(t, "ubuntu-2404")

	// A retried task must not download again. Counting the requests is the only
	// way to prove it from the outside.
	requests := 0
	h.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(qcow2test.Tiny(virtualDiskGB << 30))
	})

	if err := h.runImport(t, imported.ID); err != nil {
		t.Fatalf("second ImportHandler(): %v", err)
	}
	if requests != 0 {
		t.Errorf("the retry made %d requests, want 0: an available image must not be downloaded again",
			requests)
	}

	got, err := h.svc.Get(t.Context(), imported.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.State != image.StateAvailable {
		t.Errorf("State = %q, want available", got.State)
	}
}

func TestImportHandlerFailures(t *testing.T) {
	t.Parallel()

	t.Run("checksum mismatch leaves no volume and no staged file", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		url, _ := h.serveImage(t, qcow2test.Tiny(virtualDiskGB<<30))

		imported, err := h.svc.Import(t.Context(), image.ImportInput{
			Name:      "mismatched",
			SourceURL: url,
			Checksum: image.Checksum{
				Algorithm: image.ChecksumSHA256,
				Value:     strings.Repeat("ab", 32),
			},
		})
		if err != nil {
			t.Fatalf("Import(): %v", err)
		}

		err = h.runImport(t, imported.ID)
		if !errors.Is(err, image.ErrChecksumMismatch) {
			t.Fatalf("ImportHandler() error = %v, want ErrChecksumMismatch", err)
		}

		assertFailedAndClean(t, h, imported.ID)
	})

	t.Run("a 404 fails the image without publishing anything", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "gone", http.StatusNotFound)
		}))
		t.Cleanup(h.server.Close)

		imported, err := h.svc.Import(t.Context(), image.ImportInput{
			Name: "missing", SourceURL: h.server.URL + "/image.img",
		})
		if err != nil {
			t.Fatalf("Import(): %v", err)
		}
		if err := h.runImport(t, imported.ID); !errors.Is(err, imagefetch.ErrBadStatus) {
			t.Fatalf("ImportHandler() error = %v, want ErrBadStatus", err)
		}

		assertFailedAndClean(t, h, imported.ID)
	})

	t.Run("a body that is not an image fails before the volume exists", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		// A 4 KiB HTML error page: long enough to pass the size check, not an
		// image.
		url, _ := h.serveImage(t, []byte(strings.Repeat("<html>not an image</html>", 100)))

		imported, err := h.svc.Import(t.Context(), image.ImportInput{Name: "html", SourceURL: url})
		if err != nil {
			t.Fatalf("Import(): %v", err)
		}
		if err := h.runImport(t, imported.ID); err == nil {
			t.Fatal("ImportHandler() error = nil, want an error")
		}

		assertFailedAndClean(t, h, imported.ID)
	})

	t.Run("an oversized body is refused", func(t *testing.T) {
		t.Parallel()

		db := storetest.New(t)
		hv := fake.New(fake.Config{})
		if err := hv.EnsureInfra(t.Context(), hypervisor.InfraConfig{
			ImagesPool: imagesPool, InstancesPool: instancesPool, DataDir: t.TempDir(),
		}); err != nil {
			t.Fatalf("EnsureInfra(): %v", err)
		}
		tasks, err := task.New(task.Options{Store: db})
		if err != nil {
			t.Fatalf("task.New(): %v", err)
		}
		svc, err := image.New(image.Options{
			Logger: logging.Discard(), Store: db, Hypervisor: hv, Tasks: tasks,
			Downloader: imagefetch.New(imagefetch.Options{
				AllowPrivateNetworks: true,
				MaxBytes:             1024,
			}),
			ImagesPool: imagesPool, TempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("image.New(): %v", err)
		}

		body := qcow2test.Tiny(virtualDiskGB << 30)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write(body)
		}))
		t.Cleanup(server.Close)

		h := harness{svc: svc, store: db, hv: hv, temp: t.TempDir()}
		imported, err := svc.Import(t.Context(), image.ImportInput{
			Name: "huge", SourceURL: server.URL + "/image.img",
		})
		if err != nil {
			t.Fatalf("Import(): %v", err)
		}
		if err := h.runImport(t, imported.ID); !errors.Is(err, imagefetch.ErrTooLarge) {
			t.Fatalf("ImportHandler() error = %v, want ErrTooLarge", err)
		}
		assertFailedAndClean(t, h, imported.ID)
	})

	t.Run("a deleted image is skipped instead of failing", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		url, _ := h.serveImage(t, qcow2test.Tiny(virtualDiskGB<<30))
		imported, err := h.svc.Import(t.Context(), image.ImportInput{Name: "gone", SourceURL: url})
		if err != nil {
			t.Fatalf("Import(): %v", err)
		}
		if _, err := h.store.DeleteImage(t.Context(), sqlc.DeleteImageParams{ID: imported.ID}); err != nil {
			t.Fatalf("DeleteImage(): %v", err)
		}

		if err := h.runImport(t, imported.ID); err != nil {
			t.Errorf("ImportHandler() error = %v, want nil: a missing row must not fail the task", err)
		}
	})

	t.Run("a malformed payload is rejected", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		err := h.svc.ImportHandler(t.Context(), worker.Task{
			ID: "task-test", Type: task.TypeImageImport, Payload: []byte(`{}`),
		})
		if !errors.Is(err, image.ErrBadTaskPayload) {
			t.Errorf("ImportHandler() error = %v, want ErrBadTaskPayload", err)
		}

		err = h.svc.ImportHandler(t.Context(), worker.Task{
			ID: "task-test", Type: task.TypeImageImport, Payload: []byte(`not json`),
		})
		if err == nil {
			t.Error("ImportHandler() error = nil, want a decode error")
		}
	})
}

func TestImportValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		in        image.ImportInput
		wantField string
	}{
		{
			name:      "empty name",
			in:        image.ImportInput{Name: "", SourceURL: "https://example.com/a.img"},
			wantField: "name",
		},
		{
			name:      "name with a space",
			in:        image.ImportInput{Name: "my image", SourceURL: "https://example.com/a.img"},
			wantField: "name",
		},
		{
			name:      "name too long",
			in:        image.ImportInput{Name: strings.Repeat("a", 65), SourceURL: "https://example.com/a.img"},
			wantField: "name",
		},
		{
			name:      "missing url",
			in:        image.ImportInput{Name: "ok"},
			wantField: "source_url",
		},
		{
			name:      "file scheme",
			in:        image.ImportInput{Name: "ok", SourceURL: "file:///etc/shadow"},
			wantField: "source_url",
		},
		{
			name:      "credentials in the url",
			in:        image.ImportInput{Name: "ok", SourceURL: "https://user:pw@example.com/a.img"},
			wantField: "source_url",
		},
		{
			name:      "query string in the url",
			in:        image.ImportInput{Name: "ok", SourceURL: "https://example.com/a.img?token=1"},
			wantField: "source_url",
		},
		{
			name: "checksum without an algorithm",
			in: image.ImportInput{Name: "ok", SourceURL: "https://example.com/a.img",
				Checksum: image.Checksum{Value: strings.Repeat("ab", 32)}},
			wantField: "checksum",
		},
		{
			name: "unknown checksum algorithm",
			in: image.ImportInput{Name: "ok", SourceURL: "https://example.com/a.img",
				Checksum: image.Checksum{Algorithm: "md5", Value: strings.Repeat("ab", 16)}},
			wantField: "checksum.algorithm",
		},
		{
			name: "wrong checksum length",
			in: image.ImportInput{Name: "ok", SourceURL: "https://example.com/a.img",
				Checksum: image.Checksum{Algorithm: image.ChecksumSHA256, Value: "abcd"}},
			wantField: "checksum.value",
		},
		{
			name: "non hex checksum",
			in: image.ImportInput{Name: "ok", SourceURL: "https://example.com/a.img",
				Checksum: image.Checksum{
					Algorithm: image.ChecksumSHA256,
					Value:     strings.Repeat("zz", 32),
				}},
			wantField: "checksum.value",
		},
		{
			name: "unknown architecture",
			in: image.ImportInput{Name: "ok", SourceURL: "https://example.com/a.img",
				Architecture: "riscv64"},
			wantField: "architecture",
		},
		{
			name: "root disk below the minimum",
			in: image.ImportInput{Name: "ok", SourceURL: "https://example.com/a.img",
				MinDiskGB: -1},
			wantField: "min_disk_gb",
		},
		{
			name: "too many tags",
			in: image.ImportInput{Name: "ok", SourceURL: "https://example.com/a.img",
				Tags: manyTags(51)},
			wantField: "tags",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			_, err := h.svc.Import(t.Context(), tt.in)
			if err == nil {
				t.Fatal("Import() error = nil, want a validation error")
			}

			var invalid *service.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("Import() error = %v, want a *service.ValidationError", err)
			}
			if !hasField(invalid.Fields, tt.wantField) {
				t.Errorf("fields = %+v, want one for %q", invalid.Fields, tt.wantField)
			}
		})
	}
}

func TestImportRejectsADuplicateName(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.importTiny(t, "ubuntu-2404")

	url, _ := h.serveImage(t, qcow2test.Tiny(virtualDiskGB<<30))
	_, err := h.svc.Import(t.Context(), image.ImportInput{Name: "ubuntu-2404", SourceURL: url})
	if !errors.Is(err, service.ErrAlreadyExists) {
		t.Errorf("Import() error = %v, want ErrAlreadyExists", err)
	}
}

func TestGetListAndFilter(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	first := h.importTiny(t, "alpha")
	h.advance()
	second := h.importTiny(t, "beta")

	// Newest first by default.
	page, err := h.svc.List(t.Context(), image.ListInput{})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != second.ID || page.Items[1].ID != first.ID {
		t.Fatalf("List() = %s, want %s then %s", ids(page.Items), second.ID, first.ID)
	}
	if page.NextPageToken != nil {
		t.Errorf("NextPageToken = %q, want nil on a complete page", *page.NextPageToken)
	}

	// Oldest first.
	page, err = h.svc.List(t.Context(), image.ListInput{Sort: image.SortCreatedAsc})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if page.Items[0].ID != first.ID {
		t.Errorf("ascending first item = %q, want %q", page.Items[0].ID, first.ID)
	}

	// The query matches a substring of the name.
	page, err = h.svc.List(t.Context(), image.ListInput{Query: "alph"})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != first.ID {
		t.Errorf("filtered page = %s, want only %s", ids(page.Items), first.ID)
	}

	// The state filter sees the failed image from a refused import.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)
	failed, err := h.svc.Import(t.Context(), image.ImportInput{
		Name: "gamma", SourceURL: broken.URL + "/image.img",
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}
	if err := h.runImport(t, failed.ID); !errors.Is(err, imagefetch.ErrBadStatus) {
		t.Fatalf("ImportHandler(): error = %v, want ErrBadStatus", err)
	}

	page, err = h.svc.List(t.Context(), image.ListInput{State: image.StateFailed})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != failed.ID {
		t.Errorf("failed page = %s, want only %s", ids(page.Items), failed.ID)
	}
	if _, err := h.svc.List(t.Context(), image.ListInput{State: "bogus"}); err == nil {
		t.Error("List() error = nil, want a validation error for an unknown state")
	}

	if _, err := h.svc.List(t.Context(), image.ListInput{Sort: "name"}); err == nil {
		t.Error("List() error = nil, want a validation error for an unknown sort")
	}
	if _, err := h.svc.List(t.Context(), image.ListInput{PageSize: 1000}); err == nil {
		t.Error("List() error = nil, want a validation error for a huge page")
	}
	if _, err := h.svc.List(t.Context(), image.ListInput{PageToken: "not-a-token"}); err == nil {
		t.Error("List() error = nil, want a validation error for a forged token")
	}
}

func TestListPagesWithACursor(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	var want []string
	for i := range 3 {
		h.advance()
		want = append(want, h.importTiny(t, fmt.Sprintf("image-%d", i)).ID)
	}

	// One row at a time, newest first: three pages, two tokens, and every row
	// exactly once.
	var got []string
	var token string
	for range 4 {
		page, err := h.svc.List(t.Context(), image.ListInput{PageSize: 1, PageToken: token})
		if err != nil {
			t.Fatalf("List(): %v", err)
		}
		for _, item := range page.Items {
			got = append(got, item.ID)
		}
		if page.NextPageToken == nil {
			break
		}
		token = *page.NextPageToken
	}

	if len(got) != len(want) {
		t.Fatalf("paged through %d images, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[len(want)-1-i] {
			t.Errorf("page %d = %q, want %q", i, got[i], want[len(want)-1-i])
		}
	}
}

func TestGetReportsMissingImages(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	_, err := h.svc.Get(t.Context(), "img-00000000000000000")
	if !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestUpdateChangesOnlyNameDescriptionAndTags(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	original := h.importTiny(t, "ubuntu-2404")

	name := "ubuntu-24.04"
	description := "renamed"
	updated, err := h.svc.Update(t.Context(), original.ID, image.UpdateInput{
		Name:        &name,
		Description: &description,
		Tags:        map[string]string{"env": "staging"},
	})
	if err != nil {
		t.Fatalf("Update(): %v", err)
	}

	if updated.Name != name || updated.Description != description {
		t.Errorf("Update() = %q/%q, want %q/%q", updated.Name, updated.Description, name, description)
	}
	if updated.Tags["env"] != "staging" {
		t.Errorf("Tags = %v, want env=staging", updated.Tags)
	}
	// A patch must not be able to rewrite where the image came from.
	if updated.SourceURL == nil || *updated.SourceURL != *original.SourceURL {
		t.Errorf("SourceURL = %v, want it unchanged", updated.SourceURL)
	}
	if updated.State != image.StateAvailable || updated.Progress != 100 {
		t.Errorf("state = %q/%d, want available/100", updated.State, updated.Progress)
	}

	// Renaming onto an existing name is refused.
	other := h.importTiny(t, "debian-12")
	if _, err := h.svc.Update(t.Context(), other.ID,
		image.UpdateInput{Name: &name}); !errors.Is(err, service.ErrAlreadyExists) {
		t.Errorf("Update() error = %v, want ErrAlreadyExists", err)
	}

	// A bad name is a validation error, and the row is untouched.
	if _, err := h.svc.Update(t.Context(), other.ID,
		image.UpdateInput{Name: strPtr("not a name")}); err == nil {
		t.Error("Update() error = nil, want a validation error")
	}
	still, err := h.svc.Get(t.Context(), other.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if still.Name != "debian-12" {
		t.Errorf("Name = %q, want it unchanged after a rejected patch", still.Name)
	}

	// Tags are replaced wholesale, so an empty map removes them.
	cleared, err := h.svc.Update(t.Context(), other.ID,
		image.UpdateInput{Tags: map[string]string{}})
	if err != nil {
		t.Fatalf("Update(): %v", err)
	}
	if len(cleared.Tags) != 0 {
		t.Errorf("Tags = %v, want empty after an empty patch", cleared.Tags)
	}
}

func TestDeleteRemovesTheVolumeAndTheRow(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	img := h.importTiny(t, "ubuntu-2404")

	deleted, err := h.svc.Delete(t.Context(), img.ID)
	if err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	if deleted.State != image.StateDeleting {
		t.Errorf("State = %q, want deleting", deleted.State)
	}
	if !strings.HasPrefix(deleted.TaskID, "task-") {
		t.Errorf("TaskID = %q, want a task- prefixed id", deleted.TaskID)
	}

	// The volume is still there until the worker runs.
	if _, err := h.hv.ImageInfo(t.Context(), img.ID); err != nil {
		t.Fatalf("ImageInfo() before the task ran: %v", err)
	}

	if err := h.runDelete(t, img.ID); err != nil {
		t.Fatalf("DeleteHandler(): %v", err)
	}

	if _, err := h.hv.ImageInfo(t.Context(), img.ID); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("ImageInfo() after the task ran: error = %v, want ErrNotFound", err)
	}
	if _, err := h.svc.Get(t.Context(), img.ID); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
	assertEmptyDir(t, h.temp)
}

func TestDeleteRefusesAnImageInUse(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	img := h.importTiny(t, "ubuntu-2404")
	insertLiveInstance(t, h, "i-inuse000000000000", img.ID, "running")

	_, err := h.svc.Delete(t.Context(), img.ID)
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("Delete() error = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), img.ID) {
		t.Errorf("Delete() error = %q, want it to name the image", err)
	}

	// The task path re-checks, so a delete queued before an instance started
	// still refuses.
	if _, err := h.svc.Delete(t.Context(), img.ID); !errors.Is(err, service.ErrConflict) {
		t.Errorf("Delete() error = %v, want ErrConflict on the second attempt", err)
	}

	// The reason names the instance, so the console can tell the admin what to
	// terminate instead of showing a bare count.
	if !strings.Contains(err.Error(), "i-inuse000000000000") {
		t.Errorf("Delete() error = %q, want it to name the instance", err)
	}

	got, err := h.svc.Get(t.Context(), img.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.State != image.StateAvailable {
		t.Errorf("State = %q, want available: a refused delete must not change the state", got.State)
	}
}

// A row an earlier release left in `terminated` still references its image, and
// the foreign key would block the row delete after the volume was already gone.
// The delete is refused up front instead, so the pool and the metadata never
// disagree.
func TestDeleteRefusesAnImageAterminatedInstanceStillReferences(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	img := h.importTiny(t, "ubuntu-2404")
	insertLiveInstance(t, h, "i-old00000000000000", img.ID, "terminated")

	_, err := h.svc.Delete(t.Context(), img.ID)
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("Delete() error = %v, want ErrConflict", err)
	}
	if !strings.Contains(err.Error(), "i-old00000000000000") || !strings.Contains(err.Error(), "terminated") {
		t.Errorf("Delete() error = %q, want it to name the terminated instance", err)
	}

	// And the task path agrees, so a queued delete cannot slip through.
	if _, err := h.svc.Delete(t.Context(), img.ID); !errors.Is(err, service.ErrConflict) {
		t.Errorf("Delete() error = %v, want ErrConflict on the second attempt", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	img := h.importTiny(t, "ubuntu-2404")
	if _, err := h.svc.Delete(t.Context(), img.ID); err != nil {
		t.Fatalf("Delete(): %v", err)
	}
	if err := h.runDelete(t, img.ID); err != nil {
		t.Fatalf("DeleteHandler(): %v", err)
	}

	// A retried delete must not fail on the missing row, and deleting a row
	// that never existed must be a plain 404.
	if err := h.runDelete(t, img.ID); err != nil {
		t.Errorf("second DeleteHandler(): error = %v, want nil", err)
	}
	if _, err := h.svc.Delete(t.Context(), img.ID); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Delete() error = %v, want ErrNotFound", err)
	}
}

func TestResetFailedQueuesAFreshImport(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	body := qcow2test.Tiny(virtualDiskGB << 30)
	requests := 0
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write(body)
	}))
	t.Cleanup(h.server.Close)

	// Fail the first import with a 500, then retry against a healthy server.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	imported, err := h.svc.Import(t.Context(), image.ImportInput{
		Name: "flaky", SourceURL: broken.URL + "/image.img",
	})
	if err != nil {
		t.Fatalf("Import(): %v", err)
	}
	_ = h.runImport(t, imported.ID)
	broken.Close()

	// Point the failed row at the healthy server and retry it.
	if _, err := h.store.Exec(t.Context(),
		`UPDATE images SET source_url = ? WHERE id = ?`, h.server.URL+"/image.img", imported.ID); err != nil {
		t.Fatalf("update the source URL: %v", err)
	}

	retried, err := h.svc.ResetFailed(t.Context(), imported.ID)
	if err != nil {
		t.Fatalf("ResetFailed(): %v", err)
	}
	if retried.State != image.StatePending {
		t.Errorf("State = %q, want pending", retried.State)
	}
	if err := h.runImport(t, imported.ID); err != nil {
		t.Fatalf("ImportHandler() after the retry: %v", err)
	}
	if requests != 1 {
		t.Errorf("the retry made %d requests, want 1", requests)
	}

	got, err := h.svc.Get(t.Context(), imported.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.State != image.StateAvailable {
		t.Errorf("State = %q, want available", got.State)
	}

	// Retrying an image that is not failed is refused.
	if _, err := h.svc.ResetFailed(t.Context(), imported.ID); !errors.Is(err, service.ErrConflict) {
		t.Errorf("ResetFailed() error = %v, want ErrConflict", err)
	}
}

func TestHandlerRegistryCoversBothWorkflows(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	handlers := h.svc.Handler()
	for _, taskType := range []string{task.TypeImageImport, task.TypeImageDelete} {
		if handlers[taskType] == nil {
			t.Errorf("Handler()[%q] = nil, want a handler", taskType)
		}
	}
	if len(handlers) != 2 {
		t.Errorf("Handler() has %d entries, want 2", len(handlers))
	}
}

// catalogOSFamilies is the distribution vocabulary api/openapi.yaml enumerates
// for CatalogImage.os_family. It is duplicated here on purpose: a catalog entry
// that drifts away from the spec is a spec that lies about the API, and the only
// way to notice is to compare the two.
var catalogOSFamilies = map[string]bool{
	"ubuntu":    true,
	"debian":    true,
	"rocky":     true,
	"almalinux": true,
	"fedora":    true,
	"alpine":    true,
}

func TestCatalogIsStaticAndComplete(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	entries := h.svc.Catalog(t.Context())
	if len(entries) == 0 {
		t.Fatal("Catalog() is empty")
	}

	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.ID == "" || entry.Name == "" || entry.SourceURL == "" {
			t.Errorf("catalog entry %+v is incomplete", entry)
		}
		if !strings.HasPrefix(entry.SourceURL, "https://") {
			t.Errorf("catalog %q uses %q, want an https URL", entry.ID, entry.SourceURL)
		}
		if field := validateCatalogURL(entry.SourceURL); field != "" {
			t.Errorf("catalog %q has a %s field the import form would reject: %s",
				entry.ID, field, entry.SourceURL)
		}
		if entry.Architecture != image.ArchX86_64 && entry.Architecture != image.ArchAarch64 {
			t.Errorf("catalog %q has architecture %q", entry.ID, entry.Architecture)
		}
		// os_family is enumerated in api/openapi.yaml and rendered by the console
		// as the operating system of the image, so a value outside the vocabulary
		// is a spec that no longer describes this API.
		if !catalogOSFamilies[entry.OSFamily] {
			t.Errorf("catalog %q has os_family %q, which is not a known distribution", entry.ID, entry.OSFamily)
		}
		if seen[entry.ID] {
			t.Errorf("catalog %q appears twice", entry.ID)
		}
		seen[entry.ID] = true
	}
}

// assertFailedAndClean checks that a failed import left the row failed with a
// reason, published nothing, and staged nothing.
func assertFailedAndClean(t *testing.T, h harness, imageID string) {
	t.Helper()

	got, err := h.svc.Get(t.Context(), imageID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.State != image.StateFailed {
		t.Errorf("State = %q, want failed", got.State)
	}
	if got.StateReason == nil || *got.StateReason == "" {
		t.Error("StateReason is empty, want the cause of the failure")
	}
	if got.Format != "" || got.SizeBytes != nil {
		t.Errorf("Format = %q and SizeBytes = %v, want both unset after a failure",
			got.Format, got.SizeBytes)
	}

	if _, err := h.hv.ImageInfo(t.Context(), imageID); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("ImageInfo() error = %v, want ErrNotFound: no volume may survive", err)
	}
	assertEmptyDir(t, h.temp)
}

// assertEmptyDir fails when the staging directory still holds files.
func assertEmptyDir(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		t.Errorf("the staging directory still holds %s", entry.Name())
	}
}

// insertLiveInstance writes an instance row that boots from an image. Whether an
// image is in use is a database fact, so a bare INSERT is enough to make a delete
// refuse without booting anything.
func insertLiveInstance(t *testing.T, h harness, instanceID, imageID, state string) {
	t.Helper()

	// instances.instance_type is a foreign key, so the referenced type must exist.
	if _, err := h.store.Exec(t.Context(),
		`INSERT OR IGNORE INTO instance_types (id, vcpus, memory_mib) VALUES (?, 1, 1024)`,
		testInstanceType); err != nil {
		t.Fatalf("insert the instance type: %v", err)
	}
	if _, err := h.store.Exec(t.Context(), `
		INSERT INTO instances (
			id, name, state, image_id, instance_type, vcpus, memory_mib, root_disk_gb,
			launched_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 1, 1024, ?, ?, ?)`,
		instanceID, "in-use-"+instanceID, state, imageID, testInstanceType, virtualDiskGB,
		h.now.Format(time.RFC3339Nano), h.now.Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert the instance row: %v", err)
	}
}

// manyTags builds a tag set of size n.
func manyTags(n int) map[string]string {
	tags := make(map[string]string, n)
	for i := range n {
		tags[fmt.Sprintf("k%03d", i)] = "v"
	}
	return tags
}

func ids(images []image.Image) string {
	out := make([]string, 0, len(images))
	for _, img := range images {
		out = append(out, img.ID)
	}
	return strings.Join(out, ", ")
}

func hasField(fields []service.FieldError, name string) bool {
	for _, field := range fields {
		if field.Field == name {
			return true
		}
	}
	return false
}

func strPtr(value string) *string { return &value }

// sourceURLField is the field name a bad catalog URL would be reported under.
const sourceURLField = "source_url"

// validateCatalogURL reports which import field a catalog URL would fail, or an
// empty string when it is acceptable. It is the test's copy of the rules so the
// catalog cannot drift away from the validator unnoticed.
func validateCatalogURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return sourceURLField
	}
	switch {
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return sourceURLField
	case parsed.Host == "":
		return sourceURLField
	case parsed.User != nil:
		return sourceURLField
	case parsed.RawQuery != "":
		return sourceURLField
	}
	return ""
}
