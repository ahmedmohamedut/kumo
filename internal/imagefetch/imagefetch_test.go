package imagefetch_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ahmedmohamedut/kumo/internal/imagefetch"
)

// The guard is the security boundary of image import, so the policy is tested
// as a table over URLs rather than through the happy path only.
func TestFetchRefusesURLsOutsideThePolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		url     string
		wantErr error
	}{
		{name: "empty", url: "", wantErr: imagefetch.ErrScheme},
		{name: "no scheme", url: "cloud-images.ubuntu.com/x.img", wantErr: imagefetch.ErrScheme},
		{name: "file scheme", url: "file:///etc/passwd", wantErr: imagefetch.ErrScheme},
		{name: "ftp scheme", url: "ftp://example.com/x.img", wantErr: imagefetch.ErrScheme},
		{name: "gopher scheme", url: "gopher://example.com/x", wantErr: imagefetch.ErrScheme},
		{name: "credentials in the URL", url: "http://user:pass@example.com/x.img", wantErr: imagefetch.ErrCredentials},
		{name: "cloud metadata by name", url: "http://metadata.google.internal/computeMetadata/v1/", wantErr: imagefetch.ErrBlockedAddress},
		{name: "AWS metadata", url: "http://169.254.169.254/latest/meta-data/", wantErr: imagefetch.ErrBlockedAddress},
		{name: "AWS metadata IPv6", url: "http://[fd00:ec2::254]/latest/", wantErr: imagefetch.ErrBlockedAddress},
		{name: "Alibaba metadata", url: "http://100.100.100.200/latest/", wantErr: imagefetch.ErrBlockedAddress},
		{name: "loopback", url: "http://127.0.0.1/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "loopback by name", url: "http://localhost:8080/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "loopback by name, upper case", url: "http://LOCALHOST:8080/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "loopback by name, trailing dot", url: "http://localhost.:8080/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "loopback subdomain", url: "http://api.localhost/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "loopback, hosts file alias", url: "http://localhost.localdomain/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "loopback over IPv6", url: "http://ip6-localhost/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "private range", url: "http://10.1.2.3/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "private range 192.168", url: "http://192.168.1.10/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "private range 172.16", url: "http://172.16.0.1/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "unique local IPv6", url: "http://[fd00::1]/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "link-local IPv6", url: "http://[fe80::1]/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "IPv4-mapped metadata", url: "http://[::ffff:169.254.169.254]/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "unspecified address", url: "http://0.0.0.0/x.img", wantErr: imagefetch.ErrBlockedAddress},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// AllowPrivateNetworks is off, which is the production default.
			downloader := imagefetch.New(imagefetch.Options{})
			_, err := downloader.Fetch(context.Background(), tt.url)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Fetch(%q) error = %v, want %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

// A LAN mirror is a legitimate deployment, but only when the operator asks for
// it explicitly.
func TestFetchAllowsLoopbackOnlyWhenPrivateNetworksAreAllowed(t *testing.T) {
	t.Parallel()

	payload := []byte("an image")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	downloader := imagefetch.New(imagefetch.Options{AllowPrivateNetworks: true})
	fetch, err := downloader.Fetch(context.Background(), server.URL+"/x.img")
	if err != nil {
		t.Fatalf("Fetch(): %v", err)
	}
	defer fetch.Body.Close() //nolint:errcheck // test cleanup

	got, err := io.ReadAll(fetch.Body)
	if err != nil {
		t.Fatalf("read the body: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("body = %q, want %q", got, payload)
	}
	if fetch.FinalURL != server.URL+"/x.img" {
		t.Errorf("final url = %q, want the requested one", fetch.FinalURL)
	}
}

// A public URL that redirects into the metadata service is the classic SSRF
// bypass, so every hop is checked again.
// Validate is what the import handler calls, so the answer has to come back
// before the request is accepted: an operator who pasted "localhost" should see
// the 400, not a task that fails a second later.
func TestValidateRefusesTheSameURLsBeforeAnyRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		url     string
		wantErr error
	}{
		{name: "loopback by name", url: "http://localhost:8080/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "loopback subdomain", url: "http://api.localhost/x.img", wantErr: imagefetch.ErrBlockedAddress},
		{name: "metadata", url: "http://169.254.169.254/latest/", wantErr: imagefetch.ErrBlockedAddress},
		{name: "file scheme", url: "file:///etc/passwd", wantErr: imagefetch.ErrScheme},
		{name: "public URL", url: "https://cloud-images.ubuntu.com/noble/current/noble.img"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := imagefetch.New(imagefetch.Options{}).Validate(tt.url)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Validate(%q) error = %v, want %v", tt.url, err, tt.wantErr)
			}
		})
	}
}

// A deployment that mirrors images on the LAN opts in, and then the name is
// allowed too: the policy is one switch, not two code paths.
func TestValidateAllowsLoopbackWhenPrivateNetworksAreAllowed(t *testing.T) {
	t.Parallel()

	downloader := imagefetch.New(imagefetch.Options{AllowPrivateNetworks: true})
	if err := downloader.Validate("http://localhost:8080/x.img"); err != nil {
		t.Errorf("Validate() error = %v, want nil", err)
	}
}

func TestFetchRefusesARedirectToTheMetadataService(t *testing.T) {
	t.Parallel()

	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secret"))
	}))
	defer metadata.Close()

	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A literal 169.254.169.254 is never contacted: the guard refuses the
		// hop, so the test proves the policy, not the address.
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer public.Close()

	downloader := imagefetch.New(imagefetch.Options{AllowPrivateNetworks: true})
	_, err := downloader.Fetch(context.Background(), public.URL+"/x.img")
	if !errors.Is(err, imagefetch.ErrBlockedAddress) {
		t.Fatalf("Fetch() error = %v, want the redirect refused", err)
	}
}

func TestFetchFollowsARedirectWithinThePolicy(t *testing.T) {
	t.Parallel()

	payload := []byte("the real image")
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer final.Close()

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/image.qcow2", http.StatusMovedPermanently)
	}))
	defer entry.Close()

	downloader := imagefetch.New(imagefetch.Options{AllowPrivateNetworks: true})
	fetch, err := downloader.Fetch(context.Background(), entry.URL+"/latest.img")
	if err != nil {
		t.Fatalf("Fetch(): %v", err)
	}
	defer fetch.Body.Close() //nolint:errcheck // test cleanup

	if !strings.HasSuffix(fetch.FinalURL, "/image.qcow2") {
		t.Errorf("final url = %q, want the redirect target", fetch.FinalURL)
	}
}

func TestFetchRefusesARedirectLoop(t *testing.T) {
	t.Parallel()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL+"/again", http.StatusFound)
	}))
	defer server.Close()

	downloader := imagefetch.New(imagefetch.Options{AllowPrivateNetworks: true, MaxRedirects: 2})
	if _, err := downloader.Fetch(context.Background(), server.URL+"/start"); err == nil {
		t.Fatal("Fetch() error = nil, want the redirect loop refused")
	}
}

func TestFetchReportsANonSuccessStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no such image"))
	}))
	defer server.Close()

	downloader := imagefetch.New(imagefetch.Options{AllowPrivateNetworks: true})
	_, err := downloader.Fetch(context.Background(), server.URL+"/missing.img")
	if err == nil {
		t.Fatal("Fetch() error = nil, want the 404 reported")
	}
	// The body is quoted, so the operator can see why the CDN refused.
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "no such image") {
		t.Errorf("Fetch() error = %q, want the status and the body", err)
	}
}

func TestFetchRefusesAnImageLargerThanTheLimitBeforeDownloading(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		_, _ = w.Write(bytes.Repeat([]byte{0}, 1000000))
	}))
	defer server.Close()

	downloader := imagefetch.New(imagefetch.Options{AllowPrivateNetworks: true, MaxBytes: 1024})
	_, err := downloader.Fetch(context.Background(), server.URL+"/big.img")
	if !errors.Is(err, imagefetch.ErrTooLarge) {
		t.Fatalf("Fetch() error = %v, want ErrTooLarge", err)
	}
}

// A server that understates Content-Length must not be able to fill the disk.
func TestDownloadRefusesABodyThatOutgrowsTheLimit(t *testing.T) {
	t.Parallel()

	payload := bytes.Repeat([]byte{0xab}, 4096)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Chunked, so the size is never declared and the header check cannot
		// catch it: only the streaming limit can.
		w.Header().Set("Transfer-Encoding", "chunked")
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	downloader := imagefetch.New(imagefetch.Options{AllowPrivateNetworks: true, MaxBytes: 1024})
	var buf bytes.Buffer
	if _, err := downloader.Download(context.Background(), server.URL+"/liar.img", &buf); !errors.Is(err, imagefetch.ErrTooLarge) {
		t.Fatalf("Download() error = %v, want ErrTooLarge", err)
	}
	if buf.Len() > 1024+1 {
		t.Errorf("wrote %d bytes, want at most the limit plus the probe byte", buf.Len())
	}
}

func TestDownloadWritesTheWholeImage(t *testing.T) {
	t.Parallel()

	payload := []byte(strings.Repeat("qcow2", 256))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	downloader := imagefetch.New(imagefetch.Options{AllowPrivateNetworks: true, MaxBytes: 1 << 20})
	var buf bytes.Buffer
	written, err := downloader.Download(context.Background(), server.URL+"/x.img", &buf)
	if err != nil {
		t.Fatalf("Download(): %v", err)
	}
	if written != int64(len(payload)) {
		t.Errorf("written = %d, want %d", written, len(payload))
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Error("the downloaded bytes differ from what the server sent")
	}
}

// Without a size limit an unmeasurable body would fill the disk, so it is
// refused rather than streamed.
func TestFetchRefusesAnUnmeasurableBodyWithoutALimit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Chunked: no Content-Length at all.
		w.Header().Set("Transfer-Encoding", "chunked")
		_, _ = w.Write([]byte("streaming"))
	}))
	defer server.Close()

	downloader := imagefetch.New(imagefetch.Options{AllowPrivateNetworks: true})
	_, err := downloader.Fetch(context.Background(), server.URL+"/x.img")
	if !errors.Is(err, imagefetch.ErrNoContentLength) {
		t.Fatalf("Fetch() error = %v, want ErrNoContentLength", err)
	}
}

func TestDownloadHonorsContextCancellation(t *testing.T) {
	t.Parallel()

	stall := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1024")
		w.WriteHeader(http.StatusOK)
		// Flush the header, then stall until the test is over.
		w.(http.Flusher).Flush() //nolint:forcetypeassert,errcheck // best effort: the test only needs the header out
		select {
		case <-stall:
		case <-r.Context().Done():
		}
	}))
	defer close(stall)
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	downloader := imagefetch.New(imagefetch.Options{AllowPrivateNetworks: true, MaxBytes: 1 << 20})

	done := make(chan error, 1)
	go func() {
		var buf bytes.Buffer
		_, err := downloader.Download(ctx, server.URL+"/slow.img", &buf)
		done <- err
	}()

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Download() error = nil, want the cancellation reported")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Download() ignored the canceled context")
	}
}
