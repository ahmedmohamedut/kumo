//go:build sparkimage

// The M9.2 acceptance run: the seeds this package renders boot the M9.1 image
// (packaging/images/spark-4.3) as a master and two workers, and a client VM
// on the same network submits jobs with the client configuration an admin
// downloads (docs/design/09-spark.md sections 4 and 5).
//
//	KUMO_TEST_SPARK_IMAGE=/path/to/kumo-spark-4.3-<date>.qcow2 make test-spark-image
//
// It checks what only the real image can show: the workers register with the
// master under the cluster's secret; a job runs from the client; a client
// with another secret is refused at the authentication handshake; and a
// graceful shutdown of a worker that holds a job's cached blocks decommissions
// it -- the master reports it so, the VM powers off well before the drain
// timeout because the drain ends once the worker is empty, and the job's
// cached data still adds up after.
//
// KUMO_TEST_DATA_DIR puts the VMs' disks somewhere other than $TMPDIR. It needs
// qemu, qemu-img, xorriso and a writable /dev/kvm, and changes nothing on the
// host. The four VMs share a QEMU multicast network on loopback, with no route
// anywhere else.
package sparkcloudinit_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ahmedmohamedut/kumo/internal/service/sparkcloudinit"
	"github.com/ahmedmohamedut/kumo/internal/sparkapi"
)

const (
	bootCluster  = "spk-0b0070000000000aa"
	bootEndpoint = "boot.kumo"
	masterIP     = "10.251.0.10"
	clientIP     = "10.251.0.20"
	// drainTimeout is generous on purpose: the check is that the worker powers
	// off well before it, which only the drain ending early explains.
	drainTimeout = 3 * time.Minute
	// cachedTotal is the job's sum: 2 * (1 + ... + 200000).
	cachedTotal = "40000200000"
)

var workerIPs = []string{"10.251.0.11", "10.251.0.12"}

func TestSeedsBootASparkClusterOnTheImage(t *testing.T) {
	image := os.Getenv("KUMO_TEST_SPARK_IMAGE")
	if image == "" {
		t.Skip("set KUMO_TEST_SPARK_IMAGE to the qcow2 from packaging/images/spark-4.3 to run this")
	}
	for _, tool := range []string{"qemu-system-x86_64", "qemu-img", "xorriso"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required: %v", tool, err)
		}
	}
	work := t.TempDir()
	if dir := os.Getenv("KUMO_TEST_DATA_DIR"); dir != "" {
		var err error
		if work, err = os.MkdirTemp(dir, "spark-boot-"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// KUMO_TEST_KEEP=1 keeps a failed run's disks and consoles.
			if t.Failed() && os.Getenv("KUMO_TEST_KEEP") != "" {
				t.Logf("kept %s", work)
				return
			}
			_ = os.RemoveAll(work)
		})
	}

	secret, err := sparkcloudinit.NewSecret(nil)
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := sparkcloudinit.NewSecret(nil)

	mcast := fmt.Sprintf("230.0.%d.%d:%d", rand.IntN(250)+1, rand.IntN(250)+1, 20000+rand.IntN(20000))
	uiPort := freePort(t)
	nic := func(mac string) []string {
		id := "k" + strings.ReplaceAll(mac, ":", "")
		return []string{
			"-netdev", fmt.Sprintf("socket,id=%s,mcast=%s,localaddr=127.0.0.1", id, mcast),
			"-device", "virtio-net-pci,netdev=" + id + ",mac=" + mac,
		}
	}

	masterSeed, err := sparkcloudinit.MasterUserData(sparkcloudinit.MasterRequest{
		ClusterID: bootCluster, NodeName: "boot-master", NodeIP: masterIP, Secret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The master also gets a user-network NIC that only forwards its web UI
	// to the host, so the test reads /json/ as kumod will, and routes nothing.
	masterNICs := append(nic("52:54:00:5b:00:10"),
		"-netdev", fmt.Sprintf("user,id=h0,restrict=on,hostfwd=tcp:127.0.0.1:%d-:%d", uiPort, sparkcloudinit.MasterUIPort),
		"-device", "virtio-net-pci,netdev=h0,mac=52:54:00:5b:00:20")
	vms := []*vm{boot(t, work, image, "boot-master", 2048, masterSeed.Value(),
		netConfig("52:54:00:5b:00:10", masterIP, "52:54:00:5b:00:20"), masterNICs)}

	var workers []*vm
	for i, ip := range workerIPs {
		seed, err := sparkcloudinit.WorkerUserData(sparkcloudinit.WorkerRequest{
			NodeName: fmt.Sprintf("boot-default-%d", i+1), NodeIP: ip, MasterIP: masterIP, Secret: secret,
			Cores: 2, MemoryMiB: 1536, DrainTimeout: drainTimeout,
		})
		if err != nil {
			t.Fatal(err)
		}
		mac := fmt.Sprintf("52:54:00:5b:00:%d", 11+i)
		w := boot(t, work, image, fmt.Sprintf("boot-default-%d", i+1), 2560, seed.Value(), netConfig(mac, ip, ""), nic(mac))
		workers = append(workers, w)
		vms = append(vms, w)
	}

	clientConf, err := sparkcloudinit.ClientConfig(sparkcloudinit.ClientRequest{
		ClusterID: bootCluster, Endpoint: bootEndpoint, Secret: secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongConf, _ := sparkcloudinit.ClientConfig(sparkcloudinit.ClientRequest{
		ClusterID: bootCluster, Endpoint: bootEndpoint, Secret: wrong,
	})
	client := boot(t, work, image, "boot-client", 2560, clientSeed(t, clientConf.Value(), wrongConf.Value()),
		netConfig("52:54:00:5b:00:30", clientIP, ""), nic("52:54:00:5b:00:30"))
	vms = append(vms, client)

	start := time.Now()
	deadline := time.Now().Add(20 * time.Minute)

	// The master answers kumod's readiness read with both workers ALIVE.
	waitUntil(t, deadline, vms, "both workers ALIVE in the master's /json/", func() bool {
		st, err := masterStatus(uiPort)
		return err == nil && st.Alive() && len(st.AliveWorkers()) == 2
	})
	t.Logf("master ALIVE with 2 workers after %s", time.Since(start).Round(time.Second))

	waitUntil(t, deadline, vms, "the client's SparkPi", func() bool { return client.marker("KUMO-CLIENT-PI") != "" })
	t.Logf("SparkPi from the client: %s", client.marker("KUMO-CLIENT-PI"))
	if got := client.marker("KUMO-CLIENT-WRONG-SECRET"); got != "" && got != "refused" {
		t.Errorf("a client with another secret: %s", got)
	}

	waitUntil(t, deadline, vms, "the client's job to cache its data", func() bool {
		return client.marker("KUMO-CLIENT-CACHED") != ""
	})
	if got := client.marker("KUMO-CLIENT-WRONG-SECRET"); got != "refused" {
		t.Errorf("a client with another secret was not refused at the handshake (%q)", got)
	}
	if got := client.marker("KUMO-CLIENT-CACHED"); got != cachedTotal {
		t.Fatalf("the cached sum is %s, want %s", got, cachedTotal)
	}

	// The scale-down of S13: a graceful shutdown of the newest worker.
	victim := workers[1]
	var seen sync.Map
	stopWatch := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopWatch:
				return
			case <-time.After(300 * time.Millisecond):
			}
			if st, err := masterStatus(uiPort); err == nil {
				for _, w := range st.Workers {
					if w.Host == workerIPs[1] {
						seen.Store(w.State, true)
					}
				}
			}
		}
	}()
	shutdownAt := time.Now()
	if err := victim.powerdown(); err != nil {
		t.Fatalf("ACPI shutdown of %s: %v", victim.name, err)
	}
	select {
	case <-victim.exited:
	case <-time.After(drainTimeout + 2*time.Minute):
		t.Fatalf("%s did not power off within the drain timeout and a margin", victim.name)
	}
	took := time.Since(shutdownAt)
	close(stopWatch)
	t.Logf("%s powered off %s after its ACPI shutdown", victim.name, took.Round(time.Second))
	if took > drainTimeout-30*time.Second {
		t.Errorf("%s took %s to power off, near the drain timeout of %s: the drain did not end when the worker was empty",
			victim.name, took.Round(time.Second), drainTimeout)
	}
	var states []string
	seen.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok {
			states = append(states, s)
		}
		return true
	})
	if _, ok := seen.Load("DECOMMISSIONED"); !ok {
		t.Errorf("the master never reported %s DECOMMISSIONED (saw %v)", workerIPs[1], states)
	}

	waitUntil(t, deadline, vms[:1], "the client's job after the decommission", func() bool {
		return client.marker("KUMO-CLIENT-AFTER") != ""
	})
	if got := client.marker("KUMO-CLIENT-AFTER"); got != cachedTotal {
		t.Errorf("after the decommission the job's sum is %s, want %s", got, cachedTotal)
	}
	waitUntil(t, deadline, vms[:1], "the client to finish", func() bool { return client.printed("KUMO-CLIENT-DONE") })

	// No node printed the secret.
	for _, v := range vms {
		log, _ := os.ReadFile(v.serial)
		if strings.Contains(string(log), secret.Value()) {
			t.Errorf("%s's console printed the cluster's secret", v.name)
		}
	}
}

// clientSeed is a hand-written seed for the client VM: a stand-in for an
// instance in the VPC with a Spark client, using the downloaded configuration
// as it is.
func clientSeed(t *testing.T, clientConf, wrongConf string) string {
	t.Helper()
	script := `#!/usr/bin/env bash
set -uo pipefail
export LC_ALL=C SPARK_HOME=/opt/spark
say() { printf '%s\n' "$*" >/dev/console; }
fail() { say "client: $*"; tail -n 40 /root/*.log >/dev/console 2>&1; say "KUMO-CLIENT-FAILED"; exit 1; }
echo "` + masterIP + ` ` + bootEndpoint + `" >>/etc/hosts
n=0
for _ in $(seq 1 200); do
  n=$(curl -fsS --max-time 5 http://` + masterIP + `:8080/json/ 2>/dev/null |
    python3 -c 'import json,sys; print(sum(w["state"]=="ALIVE" for w in json.load(sys.stdin)["workers"]))' 2>/dev/null)
  [ "$n" = 2 ] && break
  sleep 3
done
[ "$n" = 2 ] || fail "workers alive: $n"
# As the client configuration's header says: the driver advertises this
# instance's address, which every worker can reach, not its hostname.
me=$(hostname -I | cut -d' ' -f1)
submit() { conf=$1; shift; /opt/spark/bin/spark-submit --properties-file "$conf" --conf "spark.driver.host=$me" "$@"; }
submit /root/client.conf --class org.apache.spark.examples.SparkPi \
  /opt/spark/examples/jars/spark-examples_2.13-4.3.0.jar 20 >/root/pi.log 2>&1
pi=$(sed -n 's/^Pi is roughly \([0-9.]*\).*/\1/p' /root/pi.log)
[ -n "$pi" ] || fail "SparkPi printed no result"
say "KUMO-CLIENT-PI=$pi"
timeout 90 /opt/spark/bin/spark-submit --properties-file /root/wrong.conf --conf "spark.driver.host=$me" --class org.apache.spark.examples.SparkPi \
  /opt/spark/examples/jars/spark-examples_2.13-4.3.0.jar 2 >/root/wrong.log 2>&1
if grep -q '^Pi is roughly' /root/wrong.log; then
  say "KUMO-CLIENT-WRONG-SECRET=ran"
elif grep -q 'AuthClientBootstrap: New auth protocol failed' /root/wrong.log && grep -q 'Failed to connect to master' /root/wrong.log; then
  say "KUMO-CLIENT-WRONG-SECRET=refused"
else
  say "KUMO-CLIENT-WRONG-SECRET=failed-otherwise"
fi
timeout 600 /opt/spark/bin/spark-submit --properties-file /root/client.conf --conf "spark.driver.host=$me" \
  /root/decommission.py >/root/decommission.log 2>&1
rc=$?
# What the driver saw of the decommission: whether the executor's blocks
# moved, or the executor was simply lost and its partitions recomputed.
{ echo "--- driver log, decommission lines ---"
  grep -iE "decommission|migrat|lost executor|executorlost|removed|replicat" /root/decommission.log | cut -c1-240 | head -40
  echo "---"; } >/dev/console
say "KUMO-CLIENT-JOB-EXIT=$rc"
[ "$rc" = 0 ] || fail "the decommission job exited $rc"
say "KUMO-CLIENT-DONE"
`
	job := `import json, time, urllib.request
from pyspark import StorageLevel
from pyspark.sql import SparkSession

con = open("/dev/console", "w")
def say(s):
    con.write(s + "\n")
    con.flush()

spark = SparkSession.builder.appName("kumo-boot-decommission").getOrCreate()
sc = spark.sparkContext
rdd = sc.parallelize(range(1, 200001), 16).map(lambda x: x * 2).persist(StorageLevel.MEMORY_ONLY)
say("KUMO-CLIENT-CACHED=%d" % rdd.sum())

# The test shuts a worker down now; wait until the master sees it leave.
deadline = time.time() + 900
while time.time() < deadline:
    workers = json.load(urllib.request.urlopen("http://` + masterIP + `:8080/json/"))["workers"]
    if any(w["state"] != "ALIVE" for w in workers):
        break
    time.sleep(1)
time.sleep(5)
say("KUMO-CLIENT-AFTER=%d" % rdd.sum())
spark.stop()
`
	doc := map[string]any{
		"ssh_pwauth": false,
		"write_files": []map[string]string{
			{"path": "/root/client.conf", "permissions": "0600", "content": clientConf},
			{"path": "/root/wrong.conf", "permissions": "0600", "content": wrongConf},
			{"path": "/root/decommission.py", "permissions": "0600", "content": job},
			{"path": "/run/kumo-client.sh", "permissions": "0700", "content": script},
		},
		"runcmd": [][]string{{"bash", "/run/kumo-client.sh"}},
	}
	body, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return "#cloud-config\n" + string(body)
}

func netConfig(mac, ip, hostMAC string) string {
	cfg := fmt.Sprintf(`version: 2
ethernets:
  kumo0:
    match: {macaddress: "%s"}
    set-name: kumo0
    addresses: [%s/24]
`, mac, ip)
	if hostMAC != "" {
		cfg += fmt.Sprintf(`  host0:
    match: {macaddress: "%s"}
    set-name: host0
    addresses: [10.0.2.15/24]
`, hostMAC)
	}
	return cfg
}

// masterStatus reads the master through internal/sparkapi, the client kumod's
// health pass uses (M9.4), so this run is that client's test against a real
// Spark 4.3 master as well.
func masterStatus(port int) (sparkapi.Status, error) {
	c, err := sparkapi.New(sparkapi.Config{Address: fmt.Sprintf("127.0.0.1:%d", port), Timeout: 3 * time.Second})
	if err != nil {
		return sparkapi.Status{}, err
	}
	defer c.Close()
	return c.Status(context.Background())
}

type vm struct {
	name   string
	serial string
	qmp    string
	exited chan struct{}
}

// marker is the last value a KUMO-* marker was printed with, "" if none.
func (v *vm) marker(name string) string {
	f, err := os.Open(v.serial)
	if err != nil {
		return ""
	}
	defer f.Close() //nolint:errcheck // a read-only file
	value := ""
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		if i := strings.Index(sc.Text(), name+"="); i >= 0 {
			value = strings.TrimSpace(strings.Fields(sc.Text()[i+len(name)+1:] + " ")[0])
		}
	}
	return value
}

// printed reports whether a bare marker, one with no value, was printed.
func (v *vm) printed(marker string) bool {
	log, _ := os.ReadFile(v.serial)
	return strings.Contains(string(log), marker)
}

// powerdown presses the VM's power button over QMP: an ACPI shutdown, which
// is what kumod's graceful stop is.
func (v *vm) powerdown() error {
	conn, err := net.DialTimeout("unix", v.qmp, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // a test's socket
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	if _, err := r.ReadString('\n'); err != nil { // the greeting
		return err
	}
	for _, cmd := range []string{`{"execute":"qmp_capabilities"}`, `{"execute":"system_powerdown"}`} {
		if _, err := io.WriteString(conn, cmd+"\n"); err != nil {
			return err
		}
		if _, err := r.ReadString('\n'); err != nil {
			return err
		}
	}
	return nil
}

func boot(t *testing.T, work, image, name string, memMiB int, userData, network string, nics []string) *vm {
	t.Helper()
	seedDir := filepath.Join(work, name+"-seed")
	if err := os.MkdirAll(seedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for f, content := range map[string]string{
		"user-data":      userData,
		"meta-data":      fmt.Sprintf("instance-id: kumo-boot-%s-%d\nlocal-hostname: %s\n", name, time.Now().UnixNano(), name),
		"network-config": network,
	} {
		if err := os.WriteFile(filepath.Join(seedDir, f), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	iso := filepath.Join(work, name+"-seed.iso")
	run(t, "xorriso", "-as", "mkisofs", "-quiet", "-V", "cidata", "-J", "-r", "-o", iso, seedDir)
	root := filepath.Join(work, name+"-root.qcow2")
	run(t, "qemu-img", "create", "-q", "-f", "qcow2", "-F", "qcow2", "-b", image, root, "12G")

	v := &vm{name: name, serial: filepath.Join(work, name+"-serial.log"),
		qmp: filepath.Join(work, name+".qmp"), exited: make(chan struct{})}
	args := []string{
		"-enable-kvm", "-cpu", "host", "-m", strconv.Itoa(memMiB), "-smp", "2",
		"-display", "none", "-monitor", "none", "-no-reboot",
		"-serial", "file:" + v.serial,
		"-qmp", "unix:" + v.qmp + ",server=on,wait=off",
		"-drive", "if=none,id=root,file=" + root + ",format=qcow2",
		"-device", "virtio-blk-pci,drive=root,bootindex=0",
		"-drive", "if=none,id=seed,file=" + iso + ",format=raw,readonly=on",
		"-device", "virtio-blk-pci,drive=seed",
	}
	cmd := exec.Command("qemu-system-x86_64", append(args, nics...)...)
	stderr, _ := os.Create(filepath.Join(work, name+"-qemu.stderr"))
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	go func() { _ = cmd.Wait(); close(v.exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-v.exited
		if t.Failed() {
			log, _ := os.ReadFile(v.serial)
			lines := strings.Split(string(log), "\n")
			t.Logf("--- %s console (last 40 lines)\n%s", name, strings.Join(lines[max(0, len(lines)-40):], "\n"))
		}
	})
	return v
}

// waitUntil polls cond, failing early when a VM reports a failed first boot
// or a failed client step.
func waitUntil(t *testing.T, deadline time.Time, vms []*vm, what string, cond func() bool) {
	t.Helper()
	for {
		if cond() {
			return
		}
		for _, v := range vms {
			log, _ := os.ReadFile(v.serial)
			for _, bad := range []string{"KUMO-SPARK-FIRSTBOOT-FAILED", "KUMO-CLIENT-FAILED"} {
				if strings.Contains(string(log), bad) {
					t.Fatalf("%s reported %s while waiting for %s", v.name, bad, what)
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(3 * time.Second)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("listener address is %T", l.Addr())
	}
	return addr.Port
}

func run(t *testing.T, name string, args ...string) {
	t.Helper()
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", name, err, out)
	}
}
