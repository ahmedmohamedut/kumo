package image_test

import (
	"errors"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/imagefmt/qcow2test"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/image"
)

// importEngine imports a golden image the way M7.2 means it to be imported,
// and runs the task so the row reaches available.
func (h *harness) importEngine(t *testing.T, name, version string) image.Image {
	t.Helper()

	body := qcow2test.Tiny(virtualDiskGB << 30)
	url, sum := h.serveImage(t, body)

	imported, err := h.svc.Import(t.Context(), image.ImportInput{
		Name:          name,
		SourceURL:     url,
		Checksum:      image.Checksum{Algorithm: image.ChecksumSHA256, Value: sum},
		Kind:          image.KindEngine,
		Engine:        image.EnginePostgres,
		EngineVersion: version,
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

func TestImportRecordsAnEngineImage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	got := h.importEngine(t, "postgres-18", "18")

	if got.Kind != image.KindEngine {
		t.Errorf("kind = %q, want %q", got.Kind, image.KindEngine)
	}
	if got.Engine == nil || *got.Engine != image.EnginePostgres {
		t.Errorf("engine = %v, want %q", got.Engine, image.EnginePostgres)
	}
	if got.EngineVersion == nil || *got.EngineVersion != "18" {
		t.Errorf("engine_version = %v, want \"18\"", got.EngineVersion)
	}
	if got.State != image.StateAvailable {
		t.Errorf("state = %q, want available", got.State)
	}
}

// An import that says nothing about kind is an ordinary image. Every client
// written before M7.2 sends exactly that body, so the default is what keeps
// them working.
func TestImportDefaultsToAUserImage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	got := h.importTiny(t, "ubuntu-24.04")

	if got.Kind != image.KindUser {
		t.Errorf("kind = %q, want %q", got.Kind, image.KindUser)
	}
	if got.Engine != nil {
		t.Errorf("engine = %v, want null on a user image", *got.Engine)
	}
	if got.EngineVersion != nil {
		t.Errorf("engine_version = %v, want null on a user image", *got.EngineVersion)
	}
}

// The point of the kind column: an engine image must not turn up on the Images
// pages, however the page is sorted, filtered or searched.
func TestListHidesEngineImages(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	user := h.importTiny(t, "ubuntu-24.04")
	h.advance()
	engine := h.importEngine(t, "postgres-18", "18")

	for _, in := range []image.ListInput{
		{},
		{Sort: image.SortCreatedAsc},
		{State: image.StateAvailable},
		{Query: "postgres"},
	} {
		page, err := h.svc.List(t.Context(), in)
		if err != nil {
			t.Fatalf("List(%+v): %v", in, err)
		}
		for _, got := range page.Items {
			if got.ID == engine.ID {
				t.Errorf("List(%+v) returned the engine image %s", in, engine.ID)
			}
			if got.Kind != image.KindUser {
				t.Errorf("List(%+v) returned kind %q", in, got.Kind)
			}
		}
	}

	page, err := h.svc.List(t.Context(), image.ListInput{})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != user.ID {
		t.Errorf("default list = %s, want only the user image %s", ids(page.Items), user.ID)
	}
}

// Asking for engine images explicitly is how the module reaches them; nothing
// else may.
func TestListEngineKindOnRequest(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.importTiny(t, "ubuntu-24.04")
	h.advance()
	engine := h.importEngine(t, "postgres-18", "18")

	page, err := h.svc.List(t.Context(), image.ListInput{Kind: image.KindEngine})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != engine.ID {
		t.Errorf("engine list = %s, want only %s", ids(page.Items), engine.ID)
	}

	if _, err := h.svc.List(t.Context(), image.ListInput{Kind: "golden"}); err == nil {
		t.Error("List() with an unknown kind returned no error")
	} else {
		var invalid *service.ValidationError
		if !errors.As(err, &invalid) {
			t.Fatalf("List() error = %v, want a validation error", err)
		}
		if len(invalid.Fields) != 1 || invalid.Fields[0].Field != "kind" {
			t.Errorf("fields = %+v, want one on kind", invalid.Fields)
		}
	}
}

// Get is by id, not discovery, and an operator debugging a database needs the
// row: hiding an engine image here would only make the module harder to
// support.
func TestGetReturnsAnEngineImage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	engine := h.importEngine(t, "postgres-18", "18")

	got, err := h.svc.Get(t.Context(), engine.ID)
	if err != nil {
		t.Fatalf("Get(): %v", err)
	}
	if got.Kind != image.KindEngine {
		t.Errorf("kind = %q, want engine", got.Kind)
	}
}

func TestEngineFieldValidation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	body := qcow2test.Tiny(virtualDiskGB << 30)
	url, sum := h.serveImage(t, body)

	base := func() image.ImportInput {
		return image.ImportInput{
			Name:      "candidate",
			SourceURL: url,
			Checksum:  image.Checksum{Algorithm: image.ChecksumSHA256, Value: sum},
		}
	}

	tests := []struct {
		name   string
		mutate func(*image.ImportInput)
		field  string
	}{
		{
			name:   "an unknown kind",
			mutate: func(in *image.ImportInput) { in.Kind = "golden" },
			field:  "kind",
		},
		{
			name: "an engine image without an engine",
			mutate: func(in *image.ImportInput) {
				in.Kind = image.KindEngine
				in.EngineVersion = "18"
			},
			field: "engine",
		},
		{
			name: "an engine image without a version",
			mutate: func(in *image.ImportInput) {
				in.Kind = image.KindEngine
				in.Engine = image.EnginePostgres
			},
			field: "engine_version",
		},
		{
			name: "an unsupported engine",
			mutate: func(in *image.ImportInput) {
				in.Kind = image.KindEngine
				in.Engine = "mysql"
				in.EngineVersion = "8"
			},
			field: "engine",
		},
		{
			name: "a version that is not a major",
			mutate: func(in *image.ImportInput) {
				in.Kind = image.KindEngine
				in.Engine = image.EnginePostgres
				in.EngineVersion = "18.6"
			},
			field: "engine_version",
		},
		{
			name:   "a user image claiming an engine",
			mutate: func(in *image.ImportInput) { in.Engine = image.EnginePostgres },
			field:  "engine",
		},
		{
			name:   "a user image claiming an engine version",
			mutate: func(in *image.ImportInput) { in.EngineVersion = "18" },
			field:  "engine_version",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := base()
			tc.mutate(&in)
			_, err := h.svc.Import(t.Context(), in)
			if err == nil {
				t.Fatal("Import() returned no error")
			}
			var invalid *service.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("Import() error = %v, want a validation error", err)
			}
			if !hasField(invalid.Fields, tc.field) {
				t.Errorf("fields = %+v, want one on %q", invalid.Fields, tc.field)
			}
		})
	}
}

func TestAvailableEnginesReportsWhatCanBoot(t *testing.T) {
	t.Parallel()

	t.Run("nothing imported", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		engines, err := h.svc.AvailableEngines(t.Context())
		if err != nil {
			t.Fatalf("AvailableEngines(): %v", err)
		}
		if len(engines) != 0 {
			t.Errorf("engines = %+v, want none", engines)
		}
	})

	t.Run("a user image is not an engine", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.importTiny(t, "ubuntu-24.04")

		engines, err := h.svc.AvailableEngines(t.Context())
		if err != nil {
			t.Fatalf("AvailableEngines(): %v", err)
		}
		if len(engines) != 0 {
			t.Errorf("engines = %+v, want none", engines)
		}
	})

	t.Run("an engine image that is still pending does not count", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		body := qcow2test.Tiny(virtualDiskGB << 30)
		url, sum := h.serveImage(t, body)
		// Imported but the task never runs, so the row stays pending.
		if _, err := h.svc.Import(t.Context(), image.ImportInput{
			Name:          "postgres-18",
			SourceURL:     url,
			Checksum:      image.Checksum{Algorithm: image.ChecksumSHA256, Value: sum},
			Kind:          image.KindEngine,
			Engine:        image.EnginePostgres,
			EngineVersion: "18",
		}); err != nil {
			t.Fatalf("Import(): %v", err)
		}

		engines, err := h.svc.AvailableEngines(t.Context())
		if err != nil {
			t.Fatalf("AvailableEngines(): %v", err)
		}
		if len(engines) != 0 {
			t.Errorf("engines = %+v, want none while the import is pending", engines)
		}
	})

	t.Run("an available engine image", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		img := h.importEngine(t, "postgres-18", "18")

		engines, err := h.svc.AvailableEngines(t.Context())
		if err != nil {
			t.Fatalf("AvailableEngines(): %v", err)
		}
		if len(engines) != 1 {
			t.Fatalf("engines = %+v, want one", engines)
		}
		got := engines[0]
		if got.Engine != image.EnginePostgres || got.Version != "18" {
			t.Errorf("engine = %q %q, want postgres 18", got.Engine, got.Version)
		}
		if got.ImageID != img.ID {
			t.Errorf("image_id = %q, want %q", got.ImageID, img.ID)
		}
		if got.ImageName != "postgres-18" {
			t.Errorf("image_name = %q, want postgres-18", got.ImageName)
		}
	})

	// A patched image is imported alongside the one it replaces, so two images
	// for one engine and version is the normal state. The newest is what a
	// create would boot, and the engine must be offered once.
	t.Run("a patched image is reported once, newest first", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		h.importEngine(t, "postgres-18-20261001", "18")
		h.advance()
		newer := h.importEngine(t, "postgres-18-20261101", "18")

		engines, err := h.svc.AvailableEngines(t.Context())
		if err != nil {
			t.Fatalf("AvailableEngines(): %v", err)
		}
		if len(engines) != 1 {
			t.Fatalf("engines = %+v, want one entry for postgres 18", engines)
		}
		if engines[0].ImageID != newer.ID {
			t.Errorf("image_id = %q, want the newest image %q", engines[0].ImageID, newer.ID)
		}
	})
}
