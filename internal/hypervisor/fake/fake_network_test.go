package fake_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
)

// natVPC is the network spec a NAT VPC becomes, the same shape the VPC service
// derives from a row.
func natVPC() hypervisor.NetworkSpec {
	return hypervisor.NetworkSpec{
		Name:    "vpc-0a1b2c3d4e5f60718",
		Bridge:  "kmb0a1b2c3d",
		CIDR:    "10.0.0.0/16",
		Gateway: "10.0.0.1",
		Mode:    hypervisor.NetworkModeNAT,
		Domain:  "production",
	}
}

func TestNetworkLifecycle(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name string
		call func(*fake.Fake) error
		want error
	}{
		{
			name: "define twice conflicts",
			call: func(f *fake.Fake) error {
				if err := f.DefineNetwork(ctx, natVPC()); err != nil {
					return err
				}
				return f.DefineNetwork(ctx, natVPC())
			},
			want: hypervisor.ErrAlreadyExists,
		},
		{
			name: "start a defined network",
			call: func(f *fake.Fake) error {
				if err := f.DefineNetwork(ctx, natVPC()); err != nil {
					return err
				}
				return f.StartNetwork(ctx, natVPC().Name)
			},
		},
		{
			name: "start an undefined network",
			call: func(f *fake.Fake) error { return f.StartNetwork(ctx, "vpc-99999999999999999") },
			want: hypervisor.ErrNotFound,
		},
		{
			name: "start an active network",
			call: func(f *fake.Fake) error {
				if err := f.DefineNetwork(ctx, natVPC()); err != nil {
					return err
				}
				if err := f.StartNetwork(ctx, natVPC().Name); err != nil {
					return err
				}
				return f.StartNetwork(ctx, natVPC().Name)
			},
			want: hypervisor.ErrInvalidState,
		},
		{
			name: "destroy an active network",
			call: func(f *fake.Fake) error {
				if err := f.DefineNetwork(ctx, natVPC()); err != nil {
					return err
				}
				if err := f.StartNetwork(ctx, natVPC().Name); err != nil {
					return err
				}
				return f.DestroyNetwork(ctx, natVPC().Name)
			},
		},
		{
			name: "destroy an inactive network",
			call: func(f *fake.Fake) error {
				if err := f.DefineNetwork(ctx, natVPC()); err != nil {
					return err
				}
				return f.DestroyNetwork(ctx, natVPC().Name)
			},
			want: hypervisor.ErrInvalidState,
		},
		{
			name: "undefine an inactive network",
			call: func(f *fake.Fake) error {
				if err := f.DefineNetwork(ctx, natVPC()); err != nil {
					return err
				}
				return f.UndefineNetwork(ctx, natVPC().Name)
			},
		},
		{
			name: "undefine an active network",
			call: func(f *fake.Fake) error {
				if err := f.DefineNetwork(ctx, natVPC()); err != nil {
					return err
				}
				if err := f.StartNetwork(ctx, natVPC().Name); err != nil {
					return err
				}
				return f.UndefineNetwork(ctx, natVPC().Name)
			},
			want: hypervisor.ErrInvalidState,
		},
		{
			name: "undefine an unknown network",
			call: func(f *fake.Fake) error { return f.UndefineNetwork(ctx, natVPC().Name) },
			want: hypervisor.ErrNotFound,
		},
		{
			name: "get an unknown network",
			call: func(f *fake.Fake) error {
				_, err := f.GetNetwork(ctx, natVPC().Name)
				return err
			},
			want: hypervisor.ErrNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := fake.New(fake.Config{})
			err := tt.call(f)
			if !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestGetNetworkReportsState(t *testing.T) {
	ctx := context.Background()
	f := fake.New(fake.Config{})

	spec := natVPC()
	if err := f.DefineNetwork(ctx, spec); err != nil {
		t.Fatalf("DefineNetwork() = %v", err)
	}

	got, err := f.GetNetwork(ctx, spec.Name)
	if err != nil {
		t.Fatalf("GetNetwork() = %v", err)
	}
	if got.Active {
		t.Error("GetNetwork() reported an active network before StartNetwork")
	}
	if got.Name != spec.Name || got.Bridge != spec.Bridge || got.CIDR != spec.CIDR ||
		got.Gateway != spec.Gateway || got.Mode != hypervisor.NetworkModeNAT || got.Domain != spec.Domain {
		t.Errorf("GetNetwork() = %+v, want the defined spec", got)
	}

	if err := f.StartNetwork(ctx, spec.Name); err != nil {
		t.Fatalf("StartNetwork() = %v", err)
	}
	got, err = f.GetNetwork(ctx, spec.Name)
	if err != nil {
		t.Fatalf("GetNetwork() = %v", err)
	}
	if !got.Active {
		t.Error("GetNetwork() reported an inactive network after StartNetwork")
	}
}

func TestDefaultModeIsNAT(t *testing.T) {
	ctx := context.Background()
	f := fake.New(fake.Config{})

	spec := natVPC()
	spec.Mode = "" // the service always sets the mode; the backend still has a default
	if err := f.DefineNetwork(ctx, spec); err != nil {
		t.Fatalf("DefineNetwork() = %v", err)
	}
	got, err := f.GetNetwork(ctx, spec.Name)
	if err != nil {
		t.Fatalf("GetNetwork() = %v", err)
	}
	if got.Mode != hypervisor.NetworkModeNAT {
		t.Errorf("GetNetwork().Mode = %q, want %q", got.Mode, hypervisor.NetworkModeNAT)
	}
}

func TestDefineNetworkRejectsMissingBridge(t *testing.T) {
	ctx := context.Background()
	f := fake.New(fake.Config{})

	spec := natVPC()
	spec.Bridge = ""
	if err := f.DefineNetwork(ctx, spec); !errors.Is(err, hypervisor.ErrInvalidSpec) {
		t.Fatalf("DefineNetwork() = %v, want ErrInvalidSpec", err)
	}
}

func TestDHCPHostsLifecycle(t *testing.T) {
	ctx := context.Background()
	f := fake.New(fake.Config{})

	spec := natVPC()
	if err := f.DefineNetwork(ctx, spec); err != nil {
		t.Fatalf("DefineNetwork() = %v", err)
	}
	if err := f.StartNetwork(ctx, spec.Name); err != nil {
		t.Fatalf("StartNetwork() = %v", err)
	}

	host := hypervisor.DHCPHost{MAC: "52:54:00:12:34:56", IP: "10.0.1.10", Name: "web-1"}
	if err := f.AddDHCPHost(ctx, spec.Name, host); err != nil {
		t.Fatalf("AddDHCPHost() = %v", err)
	}

	// The host shows up in GetNetwork and in the test accessor, and dnsmasq
	// has nothing else: Kumo is the sole allocator.
	want := []hypervisor.DHCPHost{host}
	info, err := f.GetNetwork(ctx, spec.Name)
	if err != nil {
		t.Fatalf("GetNetwork() = %v", err)
	}
	if !reflect.DeepEqual(info.DHCPHosts, want) {
		t.Errorf("GetNetwork().DHCPHosts = %+v, want %+v", info.DHCPHosts, want)
	}
	if got := f.DHCPHosts(spec.Name); !reflect.DeepEqual(got, want) {
		t.Errorf("DHCPHosts() = %+v, want %+v", got, want)
	}

	// A duplicate MAC and a stolen address are both refused.
	if err := f.AddDHCPHost(ctx, spec.Name, hypervisor.DHCPHost{
		MAC: "52:54:00:12:34:56", IP: "10.0.1.11",
	}); !errors.Is(err, hypervisor.ErrAlreadyExists) {
		t.Errorf("AddDHCPHost() duplicate MAC = %v, want ErrAlreadyExists", err)
	}
	if err := f.AddDHCPHost(ctx, spec.Name, hypervisor.DHCPHost{
		MAC: "52:54:00:12:34:57", IP: "10.0.1.10",
	}); !errors.Is(err, hypervisor.ErrAlreadyExists) {
		t.Errorf("AddDHCPHost() duplicate IP = %v, want ErrAlreadyExists", err)
	}

	// A host cannot be added while the network is down.
	if err := f.DestroyNetwork(ctx, spec.Name); err != nil {
		t.Fatalf("DestroyNetwork() = %v", err)
	}
	if err := f.AddDHCPHost(ctx, spec.Name, hypervisor.DHCPHost{MAC: "52:54:00:12:34:58", IP: "10.0.1.12"}); !errors.Is(err, hypervisor.ErrInvalidState) {
		t.Errorf("AddDHCPHost() on an inactive network = %v, want ErrInvalidState", err)
	}

	// Delete frees the lease; deleting again is a not-found, which the
	// terminate workflow tolerates.
	if err := f.StartNetwork(ctx, spec.Name); err != nil {
		t.Fatalf("StartNetwork() = %v", err)
	}
	if err := f.DeleteDHCPHost(ctx, spec.Name, host.MAC); err != nil {
		t.Fatalf("DeleteDHCPHost() = %v", err)
	}
	if got := f.DHCPHosts(spec.Name); len(got) != 0 {
		t.Errorf("DHCPHosts() = %+v after delete, want none", got)
	}
	if err := f.DeleteDHCPHost(ctx, spec.Name, host.MAC); !errors.Is(err, hypervisor.ErrNotFound) {
		t.Errorf("DeleteDHCPHost() twice = %v, want ErrNotFound", err)
	}
}

func TestNetworksListsSorted(t *testing.T) {
	ctx := context.Background()
	f := fake.New(fake.Config{})

	for _, name := range []string{"vpc-bbbbbbbbbbbbbbbbb", "vpc-aaaaaaaaaaaaaaa", "vpc-ccccccccccccccc"} {
		spec := natVPC()
		spec.Name = name
		spec.Bridge = "kmb" + name[len("vpc-"):][:8]
		if err := f.DefineNetwork(ctx, spec); err != nil {
			t.Fatalf("DefineNetwork(%s) = %v", name, err)
		}
	}

	got := f.Networks()
	if len(got) != 3 {
		t.Fatalf("Networks() = %d networks, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Name > got[i].Name {
			t.Errorf("Networks() is not sorted: %s before %s", got[i-1].Name, got[i].Name)
		}
	}
}
