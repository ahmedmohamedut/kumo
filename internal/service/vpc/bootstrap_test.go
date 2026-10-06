// Bootstrap tests: the default VPC is created exactly once, and libvirt is
// brought to the state the row describes even when the boot races. These tests
// live in the package for the same reason as service_test.go: Bootstrap swaps
// hostCollision too, and the packaged tests run sequentially.
package vpc

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/hypervisor"
	"github.com/ahmedmohamedut/kumo/internal/hypervisor/fake"
	"github.com/ahmedmohamedut/kumo/internal/logging"
	"github.com/ahmedmohamedut/kumo/internal/service"
	"github.com/ahmedmohamedut/kumo/internal/service/subnet"
	"github.com/ahmedmohamedut/kumo/internal/service/task"
	"github.com/ahmedmohamedut/kumo/internal/store"
	"github.com/ahmedmohamedut/kumo/internal/store/storetest"
)

// newBootstrapHarness builds the bootstrap stack on a fresh store. Pass a
// subnet service wired to its own store, or nil to leave the subnet step of a
// fresh default out entirely.
func newBootstrapHarness(t *testing.T, subnets *subnet.Service) harness {
	t.Helper()
	return newBootstrapHarnessOn(t, storetest.New(t), subnets)
}

// newBootstrapHarnessOn builds the stack over an explicit store, so a test can
// point the subnet service at a different database on purpose.
func newBootstrapHarnessOn(t *testing.T, db *store.Store, subnets *subnet.Service) harness {
	t.Helper()

	// A fixed, collision-free answer keeps the fresh default from depending on
	// the host the test happens to run on.
	overrideHostCollision(t, func(netip.Prefix) (string, error) { return "", nil })

	now := testClock()
	hv := fake.New(fake.Config{})
	tasks, err := task.New(task.Options{Store: db})
	if err != nil {
		t.Fatalf("task.New(): %v", err)
	}
	svc, err := New(Options{
		Logger:     logging.Discard(),
		Store:      db,
		Hypervisor: hv,
		Tasks:      tasks,
		Subnets:    subnets,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("vpc.New(): %v", err)
	}
	return harness{svc: svc, store: db, hv: hv, tasks: tasks, sub: subnets, now: &now}
}

// wiredSubnets is the subnet service a real boot wires into Bootstrap: it
// shares the very same database.
func wiredSubnets(t *testing.T, db *store.Store) *subnet.Service {
	t.Helper()

	sub, err := subnet.New(subnet.Options{Logger: logging.Discard(), Store: db})
	if err != nil {
		t.Fatalf("subnet.New(): %v", err)
	}
	return sub
}

// countSubnets is the raw count of subnet rows, for the assertions that only
// care about "none" or "one".
func countSubnets(t *testing.T, h harness) int {
	t.Helper()

	var count int
	if err := h.store.DB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM subnets").Scan(&count); err != nil {
		t.Fatalf("count the subnets: %v", err)
	}
	return count
}

func TestBootstrapReturnsTheExistingDefaultVPC(t *testing.T) {
	h := newBootstrapHarness(t, nil)

	created, err := h.svc.Create(context.Background(), CreateInput{
		Name: testVPCName, CIDR: "10.10.0.0/16",
		Tags: map[string]string{testEnvTag: testVPCName},
	})
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if _, err := h.store.Exec(context.Background(),
		"UPDATE vpcs SET is_default = 1 WHERE id = ?", created.ID); err != nil {
		t.Fatalf("mark the VPC default: %v", err)
	}

	// Even when a matching network already exists the row wins: the row is the
	// source of truth, so the adopted net must not be touched.
	if err := h.hv.DefineNetwork(context.Background(), hypervisor.NetworkSpec{
		Name: "kumo-default", Bridge: "virbr-kumo0", CIDR: "10.0.0.0/24",
		Gateway: "10.0.0.1", Mode: hypervisor.NetworkModeNAT, Domain: "lan",
	}); err != nil {
		t.Fatalf("seed the kumo-default network: %v", err)
	}

	got, err := h.svc.Bootstrap(context.Background(), "kumo-default")
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if got.ID != created.ID || got.Name != testVPCName || !got.IsDefault {
		t.Errorf("Bootstrap() = %+v, want the existing default %q", got, created.ID)
	}
	if got.Tags[testEnvTag] != testVPCName {
		t.Errorf("Tags = %v, want the stored tags", got.Tags)
	}

	// The pre-existing network is exactly as it was: still inactive, the only
	// definition around.
	networks := h.hv.Networks()
	if len(networks) != 1 || networks[0].Name != "kumo-default" || networks[0].Active {
		t.Errorf("Bootstrap() touched the network: %+v", networks)
	}
}

func TestBootstrapCreatesAFreshDefault(t *testing.T) {
	db := storetest.New(t)
	h := newBootstrapHarnessOn(t, db, wiredSubnets(t, db))

	got, err := h.svc.Bootstrap(context.Background(), "kumo-default")
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}

	if !strings.HasPrefix(got.ID, "vpc-") || len(got.ID) != 21 {
		t.Errorf("ID = %q, want a vpc- id of 17 hex", got.ID)
	}
	if got.Name != defaultVPCName || got.CIDR != defaultVPCCIDR || got.Mode != ModeNAT {
		t.Errorf("Bootstrap() = %q %q %q, want the documented default range", got.Name, got.CIDR, got.Mode)
	}
	if got.GatewayIP != "192.168.100.1" || got.Netmask != "255.255.255.0" {
		t.Errorf("gateway/netmask = %q/%q, want 192.168.100.1/255.255.255.0", got.GatewayIP, got.Netmask)
	}
	if !got.IsDefault {
		t.Error("IsDefault = false, want the boot default")
	}

	// The network exists, is up, and describes the row.
	networks := h.hv.Networks()
	if len(networks) != 1 {
		t.Fatalf("defined %d networks, want exactly one", len(networks))
	}
	net := networks[0]
	if net.Name != got.ID || net.Bridge != got.BridgeName || !net.Active {
		t.Errorf("network = %+v, want %q on %q and active", net, got.ID, got.BridgeName)
	}
	if net.CIDR != defaultVPCCIDR || net.Gateway != got.GatewayIP || net.Mode != hypervisor.NetworkModeNAT {
		t.Errorf("network range = %q %q %q, want the documented guest range", net.CIDR, net.Gateway, net.Mode)
	}

	// The default subnet halves the VPC: 192.168.100.0/25, the gateway inside
	// it, so 125 usable addresses.
	page, err := h.sub.List(context.Background(), subnet.ListInput{PageSize: 10, VpcID: got.ID})
	if err != nil {
		t.Fatalf("subnet.List() error = %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("default subnets = %+v, want exactly one", page.Items)
	}
	sub := page.Items[0]
	if sub.Name != defaultVPCName || sub.CIDR != "192.168.100.0/25" || !sub.IsDefault {
		t.Errorf("default subnet = %+v, want the half-split 192.168.100.0/25", sub)
	}
	if sub.TotalIPs != 128 || sub.UsableIPs != 125 || sub.AllocatedIPs != 0 {
		t.Errorf("subnet counts = %d/%d/%d, want 128/125/0",
			sub.TotalIPs, sub.UsableIPs, sub.AllocatedIPs)
	}

	// A second boot is a no-op with the same winner.
	again, err := h.svc.Bootstrap(context.Background(), "kumo-default")
	if err != nil {
		t.Fatalf("second Bootstrap() error = %v", err)
	}
	if again.ID != got.ID {
		t.Errorf("second Bootstrap() = %q, want the same default %q", again.ID, got.ID)
	}
	if len(h.hv.Networks()) != 1 {
		t.Errorf("the second boot defined %d networks", len(h.hv.Networks()))
	}
}

func TestBootstrapAdoptsTheConfiguredNetwork(t *testing.T) {
	db := storetest.New(t)
	h := newBootstrapHarnessOn(t, db, wiredSubnets(t, db))

	// An operator's pre-existing network: kumo-default on virbr-kumo0, DNS
	// domain "lan", serving 10.0.0.0/24 through 10.0.0.1, currently down.
	if err := h.hv.DefineNetwork(context.Background(), hypervisor.NetworkSpec{
		Name: "kumo-default", Bridge: "virbr-kumo0", CIDR: "10.0.0.0/24",
		Gateway: "10.0.0.1", Mode: hypervisor.NetworkModeNAT, Domain: "lan",
	}); err != nil {
		t.Fatalf("seed the kumo-default network: %v", err)
	}

	got, err := h.svc.Bootstrap(context.Background(), "kumo-default")
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}

	// The row records the network as it is, names and all: nothing the operator
	// chose may move.
	if got.Name != "lan" {
		t.Errorf("Name = %q, want the adopted DNS domain", got.Name)
	}
	if got.CIDR != "10.0.0.0/24" || got.GatewayIP != "10.0.0.1" || got.Netmask != "255.255.255.0" {
		t.Errorf("range = %q gw %q mask %q, want the adopted subnet",
			got.CIDR, got.GatewayIP, got.Netmask)
	}
	if got.LibvirtNetwork != "kumo-default" || got.BridgeName != "virbr-kumo0" {
		t.Errorf("LibvirtNetwork/BridgeName = %q/%q, want the adopted names",
			got.LibvirtNetwork, got.BridgeName)
	}
	if !got.IsDefault {
		t.Error("IsDefault = false, want the adopted network to be the default")
	}

	// The network was started but never redefined: its definition still carries
	// the operator's bridge and range, and the bridge the row keeps is the one
	// dnsmasq already serves.
	networks := h.hv.Networks()
	if len(networks) != 1 {
		t.Fatalf("networks = %d, want the adopted one alone", len(networks))
	}
	net := networks[0]
	if !net.Active {
		t.Error("the adopted network was not started")
	}
	if net.Bridge != "virbr-kumo0" || net.CIDR != "10.0.0.0/24" || net.Domain != "lan" {
		t.Errorf("the adopted definition changed: %+v", net)
	}

	// No subnet is guessed at for an adopted network: whatever dnsmasq already
	// allocates is the operator's business.
	if count := countSubnets(t, h); count != 0 {
		t.Errorf("the adoption created %d subnets, want none", count)
	}
}

func TestBootstrapDefaultsTheNameOfAnAdoptedNetwork(t *testing.T) {
	h := newBootstrapHarness(t, nil)

	if err := h.hv.DefineNetwork(context.Background(), hypervisor.NetworkSpec{
		Name: "kumo-default", Bridge: "virbr-kumo0", CIDR: "10.0.0.0/24",
		Gateway: "10.0.0.1", Mode: hypervisor.NetworkModeNAT,
	}); err != nil {
		t.Fatalf("seed the kumo-default network: %v", err)
	}

	got, err := h.svc.Bootstrap(context.Background(), "kumo-default")
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	if got.Name != defaultVPCName {
		t.Errorf("Name = %q, want the conventional name for a domainless network", got.Name)
	}
}

func TestBootstrapRejectsAnUnparsableAdoptedCIDR(t *testing.T) {
	h := newBootstrapHarness(t, nil)

	if err := h.hv.DefineNetwork(context.Background(), hypervisor.NetworkSpec{
		Name: "kumo-default", Bridge: "virbr-kumo0", CIDR: "banana",
		Gateway: "10.0.0.1", Mode: hypervisor.NetworkModeNAT, Domain: "lan",
	}); err != nil {
		t.Fatalf("seed the kumo-default network: %v", err)
	}

	_, err := h.svc.Bootstrap(context.Background(), "kumo-default")
	if err == nil || !strings.Contains(err.Error(), "unparsable CIDR") {
		t.Fatalf("Bootstrap() error = %v, want an unparsable-CIDR failure", err)
	}
	if countVPCs(t, h) != 0 {
		t.Error("the failed adoption wrote a row")
	}
}

func TestBootstrapFreshCreateRejectsAHostCollision(t *testing.T) {
	db := storetest.New(t)
	h := newBootstrapHarnessOn(t, db, wiredSubnets(t, db))
	overrideHostCollision(t, func(netip.Prefix) (string, error) {
		return "the host route 172.16.0.0/12 via eth0", nil
	})

	_, err := h.svc.Bootstrap(context.Background(), "kumo-default")
	if !errors.Is(err, service.ErrConflict) {
		t.Fatalf("Bootstrap() error = %v, want ErrConflict on a host collision", err)
	}
	if !strings.Contains(err.Error(), "192.168.100.0/24") {
		t.Errorf("the collision message does not name the default CIDR: %v", err)
	}
	if countVPCs(t, h) != 0 {
		t.Error("the refused boot wrote a row")
	}
}

// TestBootstrapRollsBackWhenStartNetworkFails covers the start half of the
// define-then-start sequence: the definition is removed again, so the next boot
// starts from a clean slate.
func TestBootstrapRollsBackWhenStartNetworkFails(t *testing.T) {
	db := storetest.New(t)
	h := newBootstrapHarnessOn(t, db, wiredSubnets(t, db))
	h.hv.FailAlways(hypervisor.OpStartNetwork, errBridgeDown)

	_, err := h.svc.Bootstrap(context.Background(), "kumo-default")
	if err == nil || !strings.Contains(err.Error(), "start the network") {
		t.Fatalf("Bootstrap() error = %v, want a start failure", err)
	}

	if countVPCs(t, h) != 0 {
		t.Error("the failed boot left the default row behind")
	}
	if networks := h.hv.Networks(); len(networks) != 0 {
		t.Errorf("the failed boot left %d networks behind: %+v", len(networks), networks)
	}
}

// TestBootstrapRollsBackWhenTheSubnetStepFails covers the failure after the
// network is already up. The subnet service points at a different database, so
// its VPC lookup misses: the row goes away and the next boot starts clean, and
// the network that the rollback cannot take down (it is active) stays for
// whoever owns it to reuse.
func TestBootstrapRollsBackWhenTheSubnetStepFails(t *testing.T) {
	db := storetest.New(t)
	foreign := wiredSubnets(t, storetest.New(t))
	h := newBootstrapHarnessOn(t, db, foreign)

	_, err := h.svc.Bootstrap(context.Background(), "kumo-default")
	if err == nil || !strings.Contains(err.Error(), "default subnet") {
		t.Fatalf("Bootstrap() error = %v, want a default-subnet failure", err)
	}

	if countVPCs(t, h) != 0 {
		t.Error("the failed boot left the default row behind")
	}
	networks := h.hv.Networks()
	if len(networks) != 1 || !networks[0].Active {
		t.Errorf("networks = %+v, want the active network alone (best effort)", networks)
	}
}

// TestBootstrapWithoutSubnetsLeavesNoDefaultSubnet covers the no-subnet wiring
// of bootstrap: the console just cannot launch into a boot default that has
// nowhere to put a guest.
func TestBootstrapWithoutSubnetsLeavesNoDefaultSubnet(t *testing.T) {
	h := newBootstrapHarness(t, nil)

	got, err := h.svc.Bootstrap(context.Background(), "kumo-default")
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}

	if count := countSubnets(t, h); count != 0 {
		t.Errorf("created %d subnets without a subnet service, want none", count)
	}
	if networks := h.hv.Networks(); len(networks) != 1 || !networks[0].Active {
		t.Errorf("networks = %+v, want the one active default", networks)
	}
	if got.ID == "" || !got.IsDefault {
		t.Errorf("Bootstrap() = %+v, want a boot default", got)
	}
}

// TestBootstrapResolvesTheConcurrentRaceToOneDefault drives two boots at once
// over one store. Only one physical connection exists, so the loser always runs
// its transaction after the winner committed: it either loses to the
// CIDR-overlap check, or (a network that would work across processes) to the
// unique name. Either way the database converges on one default. Bootstrap
// itself is documented to accept both outcomes.
func TestBootstrapResolvesTheConcurrentRaceToOneDefault(t *testing.T) {
	db := storetest.New(t)
	h := newBootstrapHarnessOn(t, db, wiredSubnets(t, db))

	const racers = 2
	type outcome struct {
		v   VPC
		err error
	}
	results := make([]outcome, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := h.svc.Bootstrap(context.Background(), "kumo-default")
			results[i] = outcome{v: v, err: err}
		}()
	}
	wg.Wait()

	winner := ""
	successes := 0
	for _, r := range results {
		if r.err != nil {
			if !errors.Is(r.err, service.ErrConflict) {
				t.Errorf("a racer failed outside the expected race: %v", r.err)
			}
			continue
		}
		successes++
		if winner == "" {
			winner = r.v.ID
		} else if r.v.ID != winner {
			t.Errorf("the racers picked two defaults: %q and %q", winner, r.v.ID)
		}
	}
	if successes == 0 {
		t.Fatal("no racer returned a default VPC")
	}
	if winner == "" || len(winner) != 21 {
		t.Errorf("winner = %q, want a vpc- id", winner)
	}

	// Exactly one default row and one active network, from whichever racer won.
	for _, table := range []struct {
		what string
		q    string
	}{{"default rows", "SELECT COUNT(*) FROM vpcs WHERE is_default = 1"},
		{"subnet rows", "SELECT COUNT(*) FROM subnets"}} {
		var count int
		if err := h.store.DB().QueryRowContext(context.Background(), table.q).Scan(&count); err != nil {
			t.Fatalf("count the %s: %v", table.what, err)
		}
		if count != 1 {
			t.Errorf("%s = %d, want 1", table.what, count)
		}
	}
	networks := h.hv.Networks()
	if len(networks) != 1 || !networks[0].Active || networks[0].Name != winner {
		t.Errorf("networks = %+v, want the winner's network active", networks)
	}

	// A later boot agrees with the winner.
	again, err := h.svc.Bootstrap(context.Background(), "kumo-default")
	if err != nil {
		t.Fatalf("Bootstrap() after the race error = %v", err)
	}
	if again.ID != winner {
		t.Errorf("Bootstrap() after the race = %q, want the winner %q", again.ID, winner)
	}
}

// countVPCs is the raw count of vpcs rows.
func countVPCs(t *testing.T, h harness) int {
	t.Helper()

	var count int
	if err := h.store.DB().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM vpcs").Scan(&count); err != nil {
		t.Fatalf("count the vpcs: %v", err)
	}
	return count
}
