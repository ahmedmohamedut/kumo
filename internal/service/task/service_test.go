package task_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

// errRollback is the caller's error that must abort the transaction.
var errRollback = errors.New("rollback")

var testClock = func() time.Time {
	return time.Date(2026, 4, 5, 11, 0, 0, 0, time.UTC)
}

// harness is the service under test with a clock the test can step.
type harness struct {
	svc *task.Service
	now *time.Time
}

func newHarness(t *testing.T) harness {
	t.Helper()

	now := testClock()
	svc, err := task.New(task.Options{Store: storetest.New(t), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}
	return harness{svc: svc, now: &now}
}

// create queues one task of the given type.
func (h harness) create(t *testing.T, taskType, resourceID string) task.Task {
	t.Helper()

	created, err := h.svc.Create(t.Context(), taskType, resourceID, map[string]string{"image_id": resourceID})
	if err != nil {
		t.Fatalf("Create(%q): %v", taskType, err)
	}
	return created
}

func TestNewValidatesItsDependencies(t *testing.T) {
	t.Parallel()

	if _, err := task.New(task.Options{}); !errors.Is(err, task.ErrNoStore) {
		t.Errorf("New() error = %v, want ErrNoStore", err)
	}
}

func TestCreateQueuesTheTask(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	created := h.create(t, task.TypeImageImport, "img-00000000000000000")

	if created.ID[:5] != "task-" {
		t.Errorf("ID = %q, want a task- prefixed id", created.ID)
	}
	if created.Type != task.TypeImageImport {
		t.Errorf("Type = %q, want %q", created.Type, task.TypeImageImport)
	}
	if created.State != task.StatePending {
		t.Errorf("State = %q, want pending", created.State)
	}
	if created.Progress != 0 {
		t.Errorf("Progress = %d, want 0", created.Progress)
	}
	if created.ResourceID == nil || *created.ResourceID != "img-00000000000000000" {
		t.Errorf("ResourceID = %v, want the image id", created.ResourceID)
	}
	if created.Error != nil {
		t.Errorf("Error = %v, want null for a queued task", *created.Error)
	}
	if !created.CreatedAt.Equal(testClock()) {
		t.Errorf("CreatedAt = %v, want %v", created.CreatedAt, testClock())
	}
}

func TestCreateRejectsAnEmptyType(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if _, err := h.svc.Create(t.Context(), "  ", "img-00000000000000000", nil); !errors.Is(err, task.ErrEmptyType) {
		t.Errorf("Create() error = %v, want ErrEmptyType", err)
	}
}

func TestCreateRejectsAPayloadThatIsNotJSON(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if _, err := h.svc.Create(t.Context(), task.TypeImageImport, "img-1", make(chan int)); err == nil {
		t.Error("Create() error = nil, want an encoding error")
	}
}

func TestGetReportsAMissingTask(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if _, err := h.svc.Get(t.Context(), "task-00000000000000000"); !errors.Is(err, service.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestListIsNewestFirstAndFiltersByResource(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	first := h.create(t, task.TypeImageImport, "img-00000000000000000")
	*h.now = h.now.Add(time.Second)
	second := h.create(t, task.TypeImageDelete, "img-00000000000000000")
	*h.now = h.now.Add(time.Second)
	third := h.create(t, task.TypeImageImport, "img-11111111111111111")

	page, err := h.svc.List(t.Context(), task.ListInput{})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("listed %d tasks, want 3", len(page.Items))
	}
	if page.Items[0].ID != third.ID || page.Items[1].ID != second.ID || page.Items[2].ID != first.ID {
		t.Errorf("order = %s, %s, %s; want %s, %s, %s",
			page.Items[0].ID, page.Items[1].ID, page.Items[2].ID, third.ID, second.ID, first.ID)
	}

	page, err = h.svc.List(t.Context(), task.ListInput{Sort: task.SortCreatedAsc})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if page.Items[0].ID != first.ID {
		t.Errorf("ascending first task = %q, want %q", page.Items[0].ID, first.ID)
	}

	page, err = h.svc.List(t.Context(), task.ListInput{ResourceID: "img-00000000000000000"})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("listed %d tasks for the image, want 2", len(page.Items))
	}
	if page.Items[0].ID != second.ID {
		t.Errorf("filtered first task = %q, want %q", page.Items[0].ID, second.ID)
	}

	// A resource with no tasks gives an empty page, not an error.
	page, err = h.svc.List(t.Context(), task.ListInput{ResourceID: "img-22222222222222222"})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 0 || page.Items == nil {
		t.Errorf("page = %+v, want an empty non-nil slice", page.Items)
	}
}

func TestListPagesWithACursor(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	var ids []string
	for range 4 {
		*h.now = h.now.Add(time.Second)
		ids = append(ids, h.create(t, task.TypeImageImport, "img-00000000000000000").ID)
	}

	var got []string
	var token string
	for range 5 {
		page, err := h.svc.List(t.Context(), task.ListInput{PageSize: 1, PageToken: token})
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

	if len(got) != len(ids) {
		t.Fatalf("paged through %d tasks, want %d", len(got), len(ids))
	}
	for i := range got {
		// Newest first: the last created task comes first.
		want := ids[len(ids)-1-i]
		if got[i] != want {
			t.Errorf("page %d = %q, want %q", i, got[i], want)
		}
	}
}

func TestListRejectsBadInput(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	tests := []struct {
		name  string
		input task.ListInput
		field string
	}{
		{name: "unknown sort", input: task.ListInput{Sort: "created_at:sideways"}, field: "sort"},
		{name: "page size too large", input: task.ListInput{PageSize: 5000}, field: "page_size"},
		{name: "negative page size", input: task.ListInput{PageSize: -1}, field: "page_size"},
		{name: "forged token", input: task.ListInput{PageToken: "not-a-token"}, field: "page_token"},
		{name: "token of the wrong id shape", input: task.ListInput{PageToken: "MjAyNg=="}, field: "page_token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := h.svc.List(t.Context(), tt.input)
			if err == nil {
				t.Fatal("List() error = nil, want a validation error")
			}

			var invalid *service.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("List() error = %v, want a *service.ValidationError", err)
			}
			if len(invalid.Fields) == 0 || invalid.Fields[0].Field != tt.field {
				t.Errorf("fields = %+v, want one for %q", invalid.Fields, tt.field)
			}
		})
	}
}

func TestCreateInTxRollsBackWithItsCaller(t *testing.T) {
	t.Parallel()

	db := storetest.New(t)
	svc, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}

	// A transaction that fails must not leave the task behind, or a queue would
	// claim work for an image that was never written.
	if err := db.WithTx(t.Context(), func(ctx context.Context, tx *store.Tx) error {
		if _, err := svc.CreateInTx(ctx, tx, task.TypeImageImport, "img-00000000000000000", nil); err != nil {
			return err
		}
		return errRollback
	}); !errors.Is(err, errRollback) {
		t.Fatalf("WithTx() error = %v, want the caller's error", err)
	}

	page, err := svc.List(t.Context(), task.ListInput{})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(page.Items) != 0 {
		t.Errorf("listed %d tasks after a rollback, want 0", len(page.Items))
	}
}
