// Package imagefetch downloads a disk image from a URL the user supplied. An
// image import is a server-side request to an arbitrary address, so the SSRF
// guard is the security boundary of the whole feature (AGENTS.md, security
// rules): only http(s) is allowed, and the address must not be a loopback,
// link-local, private or cloud-metadata target.
//
// The guard is applied three times, because each one alone can be bypassed:
// once on the URL the user submitted, once on every redirect hop, and once on
// the address the dialer actually connects to (which is what defeats DNS
// rebinding between the check and the connection).
package imagefetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Defaults for the downloader.
const (
	// DefaultTimeout bounds a stalled download with no bytes flowing.
	DefaultTimeout = 30 * time.Minute
	// DefaultMaxRedirects is generous but finite; a redirect loop must not
	// spin forever.
	DefaultMaxRedirects = 5

	// blockUserAgent identifies kumod to the CDN it pulls from.
	blockUserAgent = "kumo/1 (+https://github.com/ahmedmohamedut/kumo)"
)

// Sentinel errors the import workflow turns into a failed image.
var (
	// ErrScheme means the URL is not http(s).
	ErrScheme = errors.New("imagefetch: only http and https URLs can be fetched")
	// ErrCredentials means the URL carries a user:password pair, which is
	// almost always a paste accident and would end up in the database.
	ErrCredentials = errors.New("imagefetch: the URL must not carry credentials")
	// ErrBlockedAddress means the URL, or one of its redirects, points at a
	// host the control plane must not reach on the user's behalf.
	ErrBlockedAddress = errors.New("imagefetch: the URL points at a blocked address")
	// ErrTooLarge means the download exceeded the configured limit.
	ErrTooLarge = errors.New("imagefetch: the image is larger than the limit")
	// ErrNoContentLength means the server would not say how big the image is
	// and no limit was configured, so the download cannot be bounded.
	ErrNoContentLength = errors.New("imagefetch: the server did not report a size")
	// ErrBadStatus means the server answered, but not with an image.
	ErrBadStatus = errors.New("imagefetch: the server refused the download")
	// ErrNoAddresses means a hostname resolved to nothing.
	ErrNoAddresses = errors.New("imagefetch: the hostname resolved to no addresses")
)

// metadataHostnames are refused by name whatever the policy says, even if they
// resolve to a public address today, because that is how they would be reached
// tomorrow. AllowPrivateNetworks does not lift them: nobody mirrors cloud
// images on a metadata endpoint.
var metadataHostnames = map[string]struct{}{
	"metadata":                       {},
	"metadata.google.internal":       {},
	"metadata.goog":                  {},
	"instance-data":                  {},
	"instance-data.ec2.internal":     {},
	"metadata.azure.internal":        {},
	"metadata.oraclecloud.com":       {},
	"169.254.169.254":                {}, // AWS/Azure/GCP/OpenStack metadata
	"100.100.100.200":                {}, // Alibaba Cloud metadata
	"fd00:ec2::254":                  {}, // AWS IMDS over IPv6
	"metadata.packet.net":            {},
	"kubernetes.default.svc":         {},
	"kubernetes.default.svc.cluster": {},
}

// localHostnames name this machine. They are blocked by default and lifted by
// AllowPrivateNetworks, so the opt-in works the same for a name as for a
// literal address: a mirror on localhost is the case the flag exists for.
var localHostnames = map[string]struct{}{
	"localhost":             {},
	"localhost.localdomain": {},
	"ip6-localhost":         {},
	"ip6-loopback":          {},
}

// localHostnameSuffixes are refused the same way as an exact name. The host part
// is compared after lowercasing and dropping a trailing dot, which is what stops
// "LOCALHOST." and "foo.localhost" from sliding past the list.
var localHostnameSuffixes = []string{".localhost", ".localdomain"}

// Options configures the downloader.
type Options struct {
	// MaxBytes refuses a download larger than this. Zero means no limit.
	MaxBytes int64
	// Timeout bounds a stalled download. Zero means DefaultTimeout.
	Timeout time.Duration
	// AllowPrivateNetworks lifts the loopback, private and link-local blocks.
	// It exists for trusted deployments that mirror images on the LAN; the
	// default must stay false because the console is admin-only but still a
	// browser away from the metadata service.
	AllowPrivateNetworks bool
	// MaxRedirects bounds redirect hops. Zero means DefaultMaxRedirects.
	MaxRedirects int
	// Transport overrides the HTTP transport, which tests use to fail on
	// demand. Nil means a transport with the guard's dialer.
	Transport http.RoundTripper
	// Client overrides the whole HTTP client. When set, the caller owns
	// redirect and timeout policy.
	Client *http.Client
}

// Downloader fetches images under a URL policy.
type Downloader struct {
	opts Options
}

// New builds a Downloader.
func New(opts Options) *Downloader {
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxRedirects <= 0 {
		opts.MaxRedirects = DefaultMaxRedirects
	}
	return &Downloader{opts: opts}
}

// Fetch is what one successful download looks like: the open body, how many
// bytes Content-Length promised and the URL the content actually came from
// (after redirects).
type Fetch struct {
	Body          io.ReadCloser
	Size          int64
	FinalURL      string
	ContentLength int64
}

// Fetch opens rawURL and returns the response body to stream from. The caller
// closes Body. A non-2xx response is closed here and reported as an error, so
// a 404 page never lands in a temp file looking like an image.
func (d *Downloader) Fetch(ctx context.Context, rawURL string) (*Fetch, error) {
	endpoint, err := d.validate(rawURL)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("imagefetch: build the request: %w", err)
	}
	req.Header.Set("User-Agent", blockUserAgent)

	resp, err := d.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("imagefetch: GET %s: %w", rawURL, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close() //nolint:errcheck // nothing to do with an error page

		// Read a little of the body so the reason is useful, but never more.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("%w: %s returned %s: %s",
			ErrBadStatus, rawURL, resp.Status, strings.TrimSpace(string(snippet)))
	}

	size := resp.ContentLength
	if d.opts.MaxBytes > 0 {
		if size > d.opts.MaxBytes {
			resp.Body.Close() //nolint:errcheck // the download is refused
			return nil, fmt.Errorf("%w: %s advertises %d bytes, the limit is %d",
				ErrTooLarge, rawURL, size, d.opts.MaxBytes)
		}
	} else if size < 0 {
		// Without a limit an unmeasurable body would fill the disk.
		resp.Body.Close() //nolint:errcheck // the download is refused
		return nil, fmt.Errorf("%w: %s and no KUMO_IMAGE_MAX_BYTES to bound it",
			ErrNoContentLength, rawURL)
	}

	return &Fetch{
		Body:          resp.Body,
		Size:          size,
		FinalURL:      resp.Request.URL.String(),
		ContentLength: size,
	}, nil
}

// Download streams rawURL into w, refusing to write more than MaxBytes. It
// returns the number of bytes written. A body that grows past the limit is an
// error even when the server lied about Content-Length.
func (d *Downloader) Download(ctx context.Context, rawURL string, w io.Writer) (int64, error) {
	fetch, err := d.Fetch(ctx, rawURL)
	if err != nil {
		return 0, err
	}
	defer fetch.Body.Close() //nolint:errcheck // the caller only reads

	// One extra byte: enough to notice the body is too long without buffering
	// any of it.
	var reader io.Reader = fetch.Body
	if d.opts.MaxBytes > 0 {
		reader = io.LimitReader(fetch.Body, d.opts.MaxBytes+1)
	}

	written, err := io.Copy(w, reader)
	if err != nil {
		return written, fmt.Errorf("imagefetch: download %s: %w", rawURL, err)
	}
	if d.opts.MaxBytes > 0 && written > d.opts.MaxBytes {
		return written, fmt.Errorf("%w: %s exceeded %d bytes", ErrTooLarge, rawURL, d.opts.MaxBytes)
	}

	return written, nil
}

// client builds the HTTP client, wiring in the redirect and dialer policy.
func (d *Downloader) client() *http.Client {
	if d.opts.Client != nil {
		return d.opts.Client
	}

	timeout := d.opts.Timeout
	client := &http.Client{Timeout: timeout}

	if d.opts.Transport != nil {
		client.Transport = d.opts.Transport
	} else if transport, ok := defaultTransport().(*http.Transport); ok {
		clone := transport.Clone()
		clone.DialContext = d.dialContext
		client.Transport = clone
	}

	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= d.opts.MaxRedirects {
			return fmt.Errorf("%w: more than %d redirects", ErrTooLarge, d.opts.MaxRedirects)
		}
		// Every hop is checked again: a public URL that redirects to
		// 169.254.169.254 must not be followed.
		if err := d.validateEndpoint(req.URL); err != nil {
			return err
		}
		return nil
	}

	return client
}

// validate parses rawURL and applies the full URL policy.
// MaxBytes reports the download limit, so a caller that needs the number for
// its own bookkeeping (progress against a total, for instance) does not have to
// keep a second copy of it. Zero means no limit.
func (d *Downloader) MaxBytes() int64 {
	return d.opts.MaxBytes
}

// Validate applies the URL policy without fetching anything.
//
// The import endpoint calls it before it accepts a request, so an operator who
// pasted a metadata URL or a file:// path is told immediately instead of watching
// a task fail later. A hostname is only checked by name here; the address it
// resolves to is checked at dial time, because that is the only point where the
// answer cannot be raced.
func (d *Downloader) Validate(rawURL string) error {
	_, err := d.validate(rawURL)
	return err
}

func (d *Downloader) validate(rawURL string) (*url.URL, error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: the URL is empty", ErrScheme)
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("imagefetch: parse the URL: %w", err)
	}
	if err := d.validateEndpoint(parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// validateEndpoint applies the scheme and address rules to a parsed URL.
func (d *Downloader) validateEndpoint(parsed *url.URL) error {
	switch parsed.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("%w: %q is %q", ErrScheme, parsed.Redacted(), parsed.Scheme)
	}

	if parsed.User != nil {
		return fmt.Errorf("%w: %s", ErrCredentials, parsed.Redacted())
	}

	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("%w: %q has no host", ErrScheme, parsed.Redacted())
	}

	normalized := strings.ToLower(strings.TrimSuffix(host, "."))
	if _, blocked := metadataHostnames[normalized]; blocked {
		return fmt.Errorf("%w: %q is a metadata endpoint", ErrBlockedAddress, host)
	}
	// A loopback name is refused here as well as at dial time, so the answer
	// lands in the 400 the operator gets instead of a task that fails later.
	if d.namesLocalHost(normalized) {
		return fmt.Errorf("%w: %q points at this host", ErrBlockedAddress, host)
	}

	// A literal address is checked directly; a name is checked at dial time,
	// which is the only point where the answer cannot be raced.
	if addr, err := netip.ParseAddr(normalized); err == nil {
		return d.checkAddr(addr)
	}

	return nil
}

// namesLocalHost reports whether a host names this machine. A host that is a
// literal address is not a name; checkAddr decides that one.
func (d *Downloader) namesLocalHost(normalized string) bool {
	if d.opts.AllowPrivateNetworks {
		return false
	}
	if _, local := localHostnames[normalized]; local {
		return true
	}
	return slices.ContainsFunc(localHostnameSuffixes, func(suffix string) bool {
		return strings.HasSuffix(normalized, suffix)
	})
}

// dialContext connects to an address that passed the policy. It re-checks the
// address it is about to connect to, so a name that resolved to a public
// address during validation cannot be re-pointed at the metadata service in
// between.
func (d *Downloader) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("imagefetch: split %q: %w", address, err)
	}

	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("imagefetch: resolve %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrNoAddresses, host)
	}

	for _, addr := range addresses {
		if err := d.checkAddr(addr); err != nil {
			// Any blocked answer refuses the whole name: a name that has both a
			// public and a private answer is exactly the rebinding case.
			return nil, fmt.Errorf("%w: %q resolves to %s", err, host, addr)
		}
	}

	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
}

// checkAddr applies the address policy to one resolved address.
func (d *Downloader) checkAddr(addr netip.Addr) error {
	if d.opts.AllowPrivateNetworks {
		return nil
	}

	// Unmap first: ::ffff:169.254.169.254 must be judged as the IPv4 address it
	// really is, not as some innocuous IPv6 address.
	addr = addr.Unmap()
	addr = addr.WithZone("")

	switch {
	case addr.IsUnspecified():
		return fmt.Errorf("%w: %s is the unspecified address", ErrBlockedAddress, addr)
	case addr.IsLoopback():
		return fmt.Errorf("%w: %s is a loopback address", ErrBlockedAddress, addr)
	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		// 169.254.0.0/16 and fe80::/10: where the cloud metadata services live.
		return fmt.Errorf("%w: %s is link-local", ErrBlockedAddress, addr)
	case addr.IsPrivate():
		// RFC 1918 and RFC 4193, which a compromised guest network also uses.
		return fmt.Errorf("%w: %s is a private address", ErrBlockedAddress, addr)
	case addr.IsInterfaceLocalMulticast(), addr.IsMulticast():
		return fmt.Errorf("%w: %s is a multicast address", ErrBlockedAddress, addr)
	default:
		return nil
	}
}

// defaultTransport is the base transport the downloader clones. The timeouts
// mirror net/http's defaults; the context deadline is the real bound.
func defaultTransport() http.RoundTripper {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
	}
}
