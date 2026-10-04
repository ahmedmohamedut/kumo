// Package cloudinit renders the seed data an instance boots from: the NoCloud
// meta-data, network-config and user-data that go onto its seed ISO.
//
// It is deliberately free of libvirt, of the database and of the instance
// service, so every rule can be tested as a function. The rules themselves are
// those of docs/design/04-libvirt-integration.md §4.
package cloudinit

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// cloudConfigHeader must be the first line of a user-data part for cloud-init to
// treat it as a cloud-config rather than as a plain shell script.
const cloudConfigHeader = "#cloud-config"

// DefaultShell is the login shell the generated default user gets. cloud images
// ship a shell; a user that cannot log in with a key is not much of a default.
const DefaultShell = "/bin/bash"

// Sentinel errors, so the launch service can tell "this user data is not a
// cloud-config I can merge into" from every other failure without matching on
// message text.
var (
	// ErrNotCloudConfig is returned when user data carries the cloud-config
	// header but its body is not a YAML document this package can merge into.
	ErrNotCloudConfig = errors.New("cloudinit: the user data is not a mergeable cloud-config")
	// ErrNotYAML is returned when the body under the cloud-config header does not
	// parse as YAML at all.
	ErrNotYAML = errors.New("cloudinit: the user data is not valid YAML")
	// ErrDuplicateUser is returned when the document lists the default user more
	// than once: which entry wins would depend on the merge order, so the
	// operator is asked to fix the document instead.
	ErrDuplicateUser = errors.New("cloudinit: the user appears more than once in users")
)

// Request is one instance's seed data before it is rendered.
//
// PublicKey is the chosen key pair's public key, empty when the launch asked for
// none. DefaultUser is the image's default login name (images.default_user),
// empty when the image does not say.
type Request struct {
	UserData    string
	PublicKey   string
	DefaultUser string
}

// MetaData renders the NoCloud meta-data file.
//
// instance-id is what cloud-init treats as the identity of this instance, so it
// gets the Kumo id rather than the hostname: two instances may share a name
// across nodes and must not fight over one instance's state.
func MetaData(instanceID, hostname string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "instance-id: %s\n", instanceID)
	if hostname != "" {
		fmt.Fprintf(&out, "local-hostname: %s\n", hostname)
	}
	return out.String()
}

// NetworkConfig renders a NoCloud network-config v2 that asks for DHCP on every
// interface.
//
// match is a mapping, not a pattern in a string: cloud-init applies the entry to
// whichever interfaces the pattern names, so a single config is correct on a
// guest whose NIC is called ens3 (RHEL) or enp1s0 (Debian) without naming eth0.
func NetworkConfig() string {
	return `version: 2
ethernets:
  all:
    match:
      name: "en*"
    dhcp4: true
`
}

// UserData renders the user-data part of the seed.
//
// The cases follow docs/design/04-libvirt-integration.md §4:
//
//   - no user data and no key: nothing to say. The instance boots with whatever
//     login the image ships, which is the honest outcome of asking for neither.
//   - no user data, a key: a generated cloud-config that grants the key to the
//     image's default user.
//   - user data, no key: the caller's bytes, untouched.
//   - user data that is cloud-config, a key: the key is merged into the
//     caller's document, keeping every other setting they wrote.
//   - user data that is not cloud-config (a script or a MIME document), a key:
//     the key cannot be merged into something it does not understand, so both
//     travel as separate MIME parts.
//
// A non-empty result ends in exactly one newline, whatever the caller sent, so
// the seed ISO does not depend on how a form was submitted.
func UserData(req Request) (string, error) {
	trimmed := strings.TrimSpace(req.UserData)

	switch {
	case trimmed == "" && req.PublicKey == "":
		return "", nil

	case trimmed == "":
		return defaultUserConfig(req), nil

	case req.PublicKey == "":
		// Nothing to merge, so nothing to rewrite. cloud-init reads what the
		// caller wrote: a document without the cloud-config header is a script
		// to it, and second-guessing that here would only hide the mistake.
		return oneTrailingNewline(req.UserData), nil

	case IsCloudConfig(trimmed):
		merged, err := mergeKeyIntoCloudConfig(req.UserData, req)
		if err != nil {
			return "", err
		}
		return oneTrailingNewline(merged), nil

	default:
		return multipartWithKey(req), nil
	}
}

// IsCloudConfig reports whether cloud-init would read user data as a
// cloud-config.
//
// This mirrors cloud-init's own test, a prefix match on the payload after leading
// whitespace, so that a document Kumo merges is exactly a document cloud-init
// parses as one. Anything stricter would merge keys into something the guest
// then runs as a shell script.
func IsCloudConfig(userData string) bool {
	return strings.HasPrefix(strings.TrimSpace(userData), cloudConfigHeader)
}

// defaultUserConfig is the cloud-config of an instance that was given nothing
// but a key.
func defaultUserConfig(req Request) string {
	user := req.DefaultUser
	if user == "" {
		// No image default_user means no name to grant the key to. Falling back
		// to "ubuntu" would create a user that does not exist in the image and
		// cannot log in, so the key goes to the root account instead, which
		// every cloud image has.
		user = "root"
	}

	var out strings.Builder
	out.WriteString(cloudConfigHeader + "\n")
	out.WriteString("users:\n")
	fmt.Fprintf(&out, "  - name: %s\n", yamlScalar(user))
	out.WriteString("    ssh_authorized_keys:\n")
	fmt.Fprintf(&out, "      - %s\n", yamlScalar(req.PublicKey))
	fmt.Fprintf(&out, "    sudo: %s\n", yamlScalar("ALL=(ALL) NOPASSWD:ALL"))
	fmt.Fprintf(&out, "    shell: %s\n", yamlScalar(DefaultShell))
	out.WriteString("ssh_pwauth: false\n")
	return out.String()
}

// mergeKeyIntoCloudConfig adds the key to the document the caller wrote.
//
// The key goes into the entry for the image's default user, creating that entry
// when the caller did not write one. A top-level ssh_authorized_keys is left
// alone: cloud-init honors it for the default user too, so writing it would be
// a second way of saying the same thing.
func mergeKeyIntoCloudConfig(userData string, req Request) (string, error) {
	// The header is stripped before parsing and put back afterwards. Left in
	// place it would be parsed as a comment, which yaml.v3 re-emits from
	// whichever node it happened to attach to, and the caller would end up with
	// two or three headers.
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(stripHeader(userData)), &doc); err != nil {
		return "", fmt.Errorf("%w: %w", ErrNotYAML, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return "", fmt.Errorf("%w: it is empty", ErrNotCloudConfig)
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return "", fmt.Errorf("%w: the document is a %s, not a mapping", ErrNotCloudConfig, kindName(root.Kind))
	}

	name := req.DefaultUser
	if name == "" {
		name = "root"
	}

	users := mapValue(root, "users")
	if users == nil {
		users = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setMapValue(root, "users", users)
	}
	if users.Kind != yaml.SequenceNode {
		return "", fmt.Errorf("%w: users is a %s, not a list", ErrNotCloudConfig, kindName(users.Kind))
	}

	entry, err := findUser(users, name)
	if err != nil {
		return "", err
	}
	if entry == nil {
		entry = newUserEntry(name)
		users.Content = append(users.Content, entry)
	}
	if entry.Kind != yaml.MappingNode {
		return "", fmt.Errorf("%w: the user %q is a %s, not a mapping", ErrNotCloudConfig, name, kindName(entry.Kind))
	}

	keys := mapValue(entry, "ssh_authorized_keys")
	if keys == nil {
		keys = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setMapValue(entry, "ssh_authorized_keys", keys)
	}
	if keys.Kind != yaml.SequenceNode {
		return "", fmt.Errorf("%w: ssh_authorized_keys is a %s, not a list", ErrNotCloudConfig, kindName(keys.Kind))
	}
	if !containsScalar(keys, req.PublicKey) {
		keys.Content = append(keys.Content, &yaml.Node{
			Kind: yaml.ScalarNode, Tag: "!!str", Value: req.PublicKey,
		})
	}

	// cloud-config without this header is a script, so it goes back on the way
	// out, once.
	root.HeadComment = cloudConfigHeader

	rendered, err := yaml.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("cloudinit: render the merged cloud-config: %w", err)
	}
	return string(rendered), nil
}

// multipartWithKey delivers a key alongside user data that cannot be merged.
//
// This is the write_files / write-mime-multipart shape: a text/cloud-config part
// that grants the key, then the caller's bytes as a second part. cloud-init
// applies every part it understands, so a script still runs and the key still
// lands.
func multipartWithKey(req Request) string {
	boundary := "==KUMO-SEED=="
	// A part boundary must not appear in what it separates, and no part may be
	// empty, so the caller's data always follows a newline.
	part := func(contentType, body string) string {
		return fmt.Sprintf("--%s\r\nContent-Type: %s\r\n\r\n%s\r\n", boundary, contentType, body)
	}

	var out strings.Builder
	out.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n")
	out.WriteString(part("text/cloud-config", strings.TrimRight(defaultUserConfig(req), "\n")))
	out.WriteString(part(userDataContentType(req.UserData), strings.TrimRight(req.UserData, "\n")))
	out.WriteString("--" + boundary + "--\r\n")

	// The frames above are CRLF because MIME requires it, and so must every line
	// inside them: cloud-init splits the document on CRLF and would read a
	// part with LF endings as one long line.
	return toCRLF(out.String())
}

// toCRLF gives a MIME document the CRLF line endings the format requires.
func toCRLF(document string) string {
	return strings.ReplaceAll(strings.ReplaceAll(document, "\r\n", "\n"), "\n", "\r\n")
}

// userDataContentType guesses how cloud-init should treat the caller's part.
//
// A body that already declares its own Content-Type is a MIME document of their
// own making; it is delivered verbatim as text/plain so its own headers are not
// read as this part's headers. Anything else is treated as a script, which is
// what cloud-init expects for user data that is not cloud-config. The bytes are
// never rewritten, so a script keeps its shebang and a MIME document keeps its
// boundaries.
func userDataContentType(userData string) string {
	if strings.HasPrefix(strings.TrimSpace(userData), "Content-Type:") {
		return "text/plain"
	}
	return "text/x-shellscript"
}

// yamlScalar quotes a value only when it needs it, so a normal key line stays
// readable in the generated file and a hostile one stays a single token.
func yamlScalar(value string) string {
	if value == "" {
		return `""`
	}
	if strings.ContainsAny(value, ":#{}[]&*!|>'\"%@`\n\r\t") || strings.TrimSpace(value) != value {
		quoted, err := yaml.Marshal(value)
		if err != nil {
			return `""`
		}
		return strings.TrimRight(string(quoted), "\n")
	}
	return value
}

// mapValue returns the value node for a key of a mapping node.
func mapValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// setMapValue replaces or appends a key of a mapping node.
func setMapValue(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		value,
	)
}

// oneTrailingNewline normalises the end of a part.
func oneTrailingNewline(body string) string {
	return strings.TrimRight(body, "\r\n") + "\n"
}

// stripHeader removes the cloud-config header line, leaving the YAML document.
func stripHeader(userData string) string {
	body := strings.TrimLeft(userData, " \t\r\n")
	if !strings.HasPrefix(body, cloudConfigHeader) {
		return body
	}
	_, rest, found := strings.Cut(body, "\n")
	if !found {
		return ""
	}
	return rest
}

// newUserEntry builds the list item for a user the caller did not write.
func newUserEntry(name string) *yaml.Node {
	entry := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	entry.Content = append(entry.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "name"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name},
	)
	return entry
}

// findUser returns the mapping node of the user called name.
//
// A list item that names the user but is not a mapping is refused rather than
// skipped: cloud-init cannot use "- ubuntu" as a user either, so passing over it
// would leave the caller with a key that silently never lands.
//
// Two entries for the same user are refused too. Which one cloud-init keeps is
// its business, and it may not be the one Kumo added the key to.
func findUser(users *yaml.Node, name string) (*yaml.Node, error) {
	var found *yaml.Node
	for _, item := range users.Content {
		if item.Kind != yaml.MappingNode {
			if item.Value == name {
				return nil, fmt.Errorf("%w: the user %q is a %s, not a mapping",
					ErrNotCloudConfig, name, kindName(item.Kind))
			}
			continue
		}
		n := mapValue(item, "name")
		if n == nil || n.Value != name {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateUser, name)
		}
		found = item
	}
	return found, nil
}

// containsScalar reports whether a sequence already holds a value.
func containsScalar(sequence *yaml.Node, value string) bool {
	for _, item := range sequence.Content {
		if item.Value == value {
			return true
		}
	}
	return false
}

// kindName describes a YAML node kind for an error message.
func kindName(kind yaml.Kind) string {
	switch kind {
	case yaml.MappingNode:
		return "mapping"
	case yaml.SequenceNode:
		return "list"
	case yaml.ScalarNode:
		return "value"
	case yaml.AliasNode:
		return "alias"
	default:
		return "document"
	}
}
