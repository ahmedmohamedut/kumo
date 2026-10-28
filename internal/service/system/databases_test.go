package system_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
)

// errImagesUnreadable stands in for a store that cannot answer, so a test can
// assert the status payload survives it.
var errImagesUnreadable = errors.New("database is locked")

func engineLister(engines ...system.DatabaseEngineInfo) system.EngineLister {
	return func(context.Context) ([]system.DatabaseEngineInfo, error) { return engines, nil }
}

func TestInfoReportsTheDatabasesModule(t *testing.T) {
	t.Parallel()

	postgres18 := system.DatabaseEngineInfo{Engine: "postgres", Version: "18"}

	tests := []struct {
		name    string
		enabled bool
		lister  system.EngineLister
		want    system.DatabasesInfo
	}{
		{
			// The default of a node nobody configured: on, and nothing to
			// create from until an engine image is imported. Not an error.
			name:    "on with no engine image",
			enabled: true,
			lister:  engineLister(),
			want:    system.DatabasesInfo{Enabled: true, Engines: []system.DatabaseEngineInfo{}},
		},
		{
			name:    "on with an engine",
			enabled: true,
			lister:  engineLister(postgres18),
			want:    system.DatabasesInfo{Enabled: true, Engines: []system.DatabaseEngineInfo{postgres18}},
		},
		{
			// With the module off the engines are not even read: the routes
			// answer 503, so the answer cannot be acted on.
			name:    "off",
			enabled: false,
			lister:  engineLister(postgres18),
			want:    system.DatabasesInfo{Enabled: false, Engines: []system.DatabaseEngineInfo{}},
		},
		{
			// A node wired without an image service, which is how the API
			// tests build one.
			name:    "on without a lister",
			enabled: true,
			lister:  nil,
			want:    system.DatabasesInfo{Enabled: true, Engines: []system.DatabaseEngineInfo{}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			svc := system.New(logging.Discard(), "test", "fake", nil, nil, nil)
			svc.SetDatabases(tc.enabled, tc.lister)

			info, err := svc.Info(t.Context())
			if err != nil {
				t.Fatalf("Info(): %v", err)
			}
			if !reflect.DeepEqual(info.Databases, tc.want) {
				t.Errorf("databases = %+v, want %+v", info.Databases, tc.want)
			}
		})
	}
}

// The console polls /system/info to decide what to render, so an unreadable
// image table must not take the whole payload down with it.
func TestInfoSurvivesAnEngineListingFailure(t *testing.T) {
	t.Parallel()

	svc := system.New(logging.Discard(), "test", "fake", nil, nil, nil)
	svc.SetDatabases(true, func(context.Context) ([]system.DatabaseEngineInfo, error) {
		return nil, errImagesUnreadable
	})

	info, err := svc.Info(t.Context())
	if err != nil {
		t.Fatalf("Info() = %v, want the payload despite the listing failure", err)
	}
	if !info.Databases.Enabled {
		t.Error("enabled = false, want true: the configuration did not change")
	}
	if len(info.Databases.Engines) != 0 {
		t.Errorf("engines = %+v, want none", info.Databases.Engines)
	}
}

// A JSON null would make "no engine image yet" an error case for the console,
// which reads this as a list.
func TestDatabasesEnginesAreNeverNull(t *testing.T) {
	t.Parallel()

	svc := system.New(logging.Discard(), "test", "fake", nil, nil, nil)

	info, err := svc.Info(t.Context())
	if err != nil {
		t.Fatalf("Info(): %v", err)
	}
	if info.Databases.Engines == nil {
		t.Error("engines = nil, want an empty list")
	}
}
