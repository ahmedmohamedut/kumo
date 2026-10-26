package fake_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/objectstore"
	"github.com/ahmedmohamedut/kumo/internal/objectstore/fake"
	"github.com/ahmedmohamedut/kumo/internal/objectstore/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, fake.New(), storetest.Options{PublicURL: fake.PublicURL})
}

func TestDown(t *testing.T) {
	ctx := context.Background()
	store := fake.New()
	if err := store.CreateBucket(ctx, "logs"); err != nil {
		t.Fatal(err)
	}

	store.SetDown(true)
	if err := store.Ping(ctx); !errors.Is(err, objectstore.ErrUnavailable) {
		t.Errorf("Ping while down: got %v, want ErrUnavailable", err)
	}
	if _, err := store.ListBuckets(ctx); !errors.Is(err, objectstore.ErrUnavailable) {
		t.Errorf("ListBuckets while down: got %v, want ErrUnavailable", err)
	}
	if _, err := store.HeadBucket(ctx, "logs"); !errors.Is(err, objectstore.ErrUnavailable) {
		t.Errorf("HeadBucket while down: got %v, want ErrUnavailable", err)
	}

	store.SetDown(false)
	if err := store.Ping(ctx); err != nil {
		t.Errorf("Ping after recovery: %v", err)
	}
}

func TestCORSRecorded(t *testing.T) {
	ctx := context.Background()
	store := fake.New()
	if err := store.CreateBucket(ctx, "assets"); err != nil {
		t.Fatal(err)
	}
	origins := []string{"https://kumo.example"}
	if err := store.SetCORS(ctx, "assets", origins); err != nil {
		t.Fatal(err)
	}
	if got := store.CORS("assets"); !slices.Equal(got, origins) {
		t.Errorf("CORS: got %v, want %v", got, origins)
	}
}
