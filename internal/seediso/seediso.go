// Package seediso builds the NoCloud seed ISO that carries an instance's
// cloud-init data.
//
// It is a pure function of the rendered documents: no libvirt, no database, no
// instance service. The service renders the three documents with
// internal/service/cloudinit and hands them over; this package only wraps them in
// an ISO that cloud-init recognizes.
//
// cloud-init recognizes a NoCloud datasource by the volume label cidata or
// CIDATA, so that label is not a choice. The file names are likewise fixed by
// the datasource: user-data, meta-data and network-config.
package seediso

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kdomanski/iso9660"
)

// VolumeLabel is the label cloud-init looks for on a NoCloud seed. It writes it
// lowercase in the examples; the datasource compares case-insensitively.
const VolumeLabel = "cidata"

// The file names inside the image. cloud-init matches these names, so they are
// constants rather than parameters.
const (
	metaDataFile    = "meta-data"
	userDataFile    = "user-data"
	networkFileName = "network-config"
)

// Documents are the three rendered files of a NoCloud seed.
//
// An empty string means "do not include this file". A seed with no user-data is
// still a valid seed: cloud-init reads meta-data, applies network-config and
// creates the distro default user, which is exactly what an instance launched
// without user data should get.
type Documents struct {
	MetaData      string
	UserData      string
	NetworkConfig string
}

// Empty reports whether the seed would contain no files at all.
//
// It exists because a seed ISO with no files is not a mistake worth making: it is
// a launch that boots with no identity and no network, and the operator has no
// way to tell from the outside.
func (d Documents) Empty() bool {
	return d.MetaData == "" && d.UserData == "" && d.NetworkConfig == ""
}

// Build writes the seed ISO to path.
//
// The image is assembled in memory and written in one pass, so a failure part way
// through leaves no half-written ISO at a path the caller is about to attach to a
// domain. An existing file at path is replaced, which is what makes a retry after
// a failed launch work.
func Build(path string, docs Documents) error {
	if path == "" {
		return fmt.Errorf("%w: the seed ISO needs a path", os.ErrInvalid)
	}
	if docs.Empty() {
		return fmt.Errorf("%w: refusing to write an empty seed ISO to %s", os.ErrInvalid, path)
	}

	image, err := render(docs)
	if err != nil {
		return err
	}

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("seediso: create %s: %w", dir, err)
		}
	}

	return writeFile(path, image)
}

// render builds the ISO in memory.
func render(docs Documents) ([]byte, error) {
	writer, err := iso9660.NewWriter()
	if err != nil {
		return nil, fmt.Errorf("seediso: create the image writer: %w", err)
	}
	defer func() {
		// The staging directory is a temp dir the writer created; failing to
		// remove it is not the caller's problem and cannot corrupt the image.
		_ = writer.Cleanup()
	}()

	files := []struct {
		name    string
		content string
	}{
		{metaDataFile, docs.MetaData},
		{userDataFile, docs.UserData},
		{networkFileName, docs.NetworkConfig},
	}
	for _, file := range files {
		if file.content == "" {
			continue
		}
		if err := writer.AddFile(bytes.NewReader([]byte(file.content)), file.name); err != nil {
			return nil, fmt.Errorf("seediso: stage %s: %w", file.name, err)
		}
	}

	var image bytes.Buffer
	if err := writer.WriteTo(&image, VolumeLabel); err != nil {
		return nil, fmt.Errorf("seediso: write the image: %w", err)
	}
	return image.Bytes(), nil
}

// writeFile replaces path with the image.
func writeFile(path string, image []byte) error {
	// O_TRUNC on the final path would leave a truncated ISO behind if the write
	// failed halfway. Writing beside it and renaming means the path either holds
	// the previous image or the new one, never a fragment.
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("seediso: create a temporary file next to %s: %w", path, err)
	}
	tempName := temp.Name()
	defer func() {
		_ = os.Remove(tempName)
	}()

	if _, err := temp.Write(image); err != nil {
		_ = temp.Close()
		return fmt.Errorf("seediso: write %s: %w", path, err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("seediso: sync %s: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("seediso: close %s: %w", path, err)
	}
	// 0644: the ISO carries user data and a public key, not a secret, but it is
	// still only the daemon's own to read.
	if err := os.Chmod(tempName, 0o644); err != nil {
		return fmt.Errorf("seediso: set the mode of %s: %w", path, err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("seediso: put the seed ISO at %s: %w", path, err)
	}
	return nil
}
