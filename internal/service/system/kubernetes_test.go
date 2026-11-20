package system_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service/system"
)

var errListing = errors.New("the image table is locked")

// The Kubernetes block of /system/info (docs/design/08-kubernetes.md section
// 2): the switch, and the versions a cluster can be created at. Versions is a
// list, never null, and a listing failure reports none rather than failing the
// payload the console polls.
func TestInfoReportsTheKubernetesModule(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		enabled bool
		lister  system.VersionLister
		want    []string
	}{
		"off": {enabled: false, lister: func(context.Context) ([]string, error) {
			return []string{"1.37"}, nil
		}, want: []string{}},
		"on with no image": {enabled: true, lister: func(context.Context) ([]string, error) {
			return nil, nil
		}, want: []string{}},
		"on with images": {enabled: true, lister: func(context.Context) ([]string, error) {
			return []string{"1.38", "1.37"}, nil
		}, want: []string{"1.38", "1.37"}},
		"a listing failure": {enabled: true, lister: func(context.Context) ([]string, error) {
			return nil, errListing
		}, want: []string{}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			svc := system.New(logging.Discard(), "v0.0.1", "fake", nil, nil, nil)
			svc.SetKubernetes(tc.enabled, tc.lister)
			info, err := svc.Info(t.Context())
			if err != nil {
				t.Fatalf("Info(): %v", err)
			}
			if info.Kubernetes.Enabled != tc.enabled {
				t.Errorf("enabled = %v, want %v", info.Kubernetes.Enabled, tc.enabled)
			}
			if info.Kubernetes.Versions == nil {
				t.Fatal("versions is nil, want a list")
			}
			if len(info.Kubernetes.Versions) != len(tc.want) {
				t.Fatalf("versions = %v, want %v", info.Kubernetes.Versions, tc.want)
			}
			for i := range tc.want {
				if info.Kubernetes.Versions[i] != tc.want[i] {
					t.Errorf("versions = %v, want %v", info.Kubernetes.Versions, tc.want)
				}
			}
		})
	}
}

// The Spark block mirrors the Kubernetes one, and says whether a cluster can
// have object storage on this node (docs/design/09-spark.md section 2).
func TestInfoReportsTheSparkModule(t *testing.T) {
	t.Parallel()
	svc := system.New(logging.Discard(), "v0.0.1", "fake", nil, nil, nil)
	info, err := svc.Info(t.Context())
	if err != nil || info.Spark.Enabled || info.Spark.Versions == nil || info.Spark.ObjectStorage {
		t.Fatalf("an unset module = %+v, %v; want off, no versions, no object storage", info.Spark, err)
	}
	svc.SetSpark(true, func(context.Context) ([]string, error) { return []string{"4.3"}, nil }, true)
	info, err = svc.Info(t.Context())
	if err != nil || !info.Spark.Enabled || len(info.Spark.Versions) != 1 || info.Spark.Versions[0] != "4.3" ||
		!info.Spark.ObjectStorage {
		t.Errorf("on = %+v, %v", info.Spark, err)
	}
	svc.SetSpark(true, func(context.Context) ([]string, error) { return nil, errListing }, false)
	info, _ = svc.Info(t.Context())
	if info.Spark.Versions == nil || len(info.Spark.Versions) != 0 || info.Spark.ObjectStorage {
		t.Errorf("a listing failure = %+v, want an empty list", info.Spark)
	}
}
