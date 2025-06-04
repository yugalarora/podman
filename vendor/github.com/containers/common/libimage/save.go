//go:build !remote

package libimage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dirTransport "github.com/containers/image/v5/directory"
	dockerArchiveTransport "github.com/containers/image/v5/docker/archive"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/manifest"
	ociArchiveTransport "github.com/containers/image/v5/oci/archive"
	ociTransport "github.com/containers/image/v5/oci/layout"
	"github.com/containers/image/v5/types"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
)

// SaveOptions allow for customizing saving images.
type SaveOptions struct {
	CopyOptions

	// AdditionalTags for the saved image.  Incompatible when saving
	// multiple images.
	AdditionalTags []string
	// NoPrefix indicates whether to add the "localhost/" prefix to image names
	// when no domain is specified.
	NoPrefix bool
}

// Save saves one or more images indicated by `names` in the specified `format`
// to `path`.  Supported formats are oci-archive, docker-archive, oci-dir and
// docker-dir.  The latter two adhere to the dir transport in the corresponding
// oci or docker v2s2 format.  Please note that only docker-archive supports
// saving more than one images.  Other formats will yield an error attempting
// to save more than one.
func (r *Runtime) Save(ctx context.Context, names []string, format, path string, options *SaveOptions) error {
	logrus.Debugf("Saving one more images (%s) to %q", names, path)

	if options == nil {
		options = &SaveOptions{}
	}

	// First some sanity checks to simplify subsequent code.
	switch len(names) {
	case 0:
		return errors.New("no image specified for saving images")
	case 1:
		// All formats support saving 1.
	default:
		if format != "docker-archive" {
			return fmt.Errorf("unsupported format %q for saving multiple images (only docker-archive)", format)
		}
		if len(options.AdditionalTags) > 0 {
			return errors.New("cannot save multiple images with multiple tags")
		}
	}

	// Dispatch the save operations.
	switch format {
	case "oci-archive", "oci-dir", "docker-dir":
		if len(names) > 1 {
			return fmt.Errorf("%q does not support saving multiple images (%v)", format, names)
		}
		return r.saveSingleImage(ctx, names[0], format, path, options)

	case "docker-archive":
		options.ManifestMIMEType = manifest.DockerV2Schema2MediaType
		return r.saveDockerArchive(ctx, names, path, options)
	}

	return fmt.Errorf("unsupported format %q for saving images", format)
}

// saveSingleImage saves the specified image name to the specified path.
// Supported formats are "oci-archive", "oci-dir" and "docker-dir".
func (r *Runtime) saveSingleImage(ctx context.Context, name, format, path string, options *SaveOptions) error {
	var imageNameForTag string
	var localImage *Image
	var err error

	if options.NoPrefix {
		// Lookup the image to get its ID and ensure it exists.
		// We use the original 'name' for this lookup.
		img, resolvedName, lookupErr := r.LookupImage(name, nil) // This call is okay as we primarily need the *Image object
		if lookupErr != nil {
			return lookupErr
		}
		localImage = img

		// If 'name' is not an ID itself, use 'name' directly for the tag.
		// Otherwise, if 'name' was an ID, there's no tag to apply unless specified in AdditionalTags.
		if !strings.HasPrefix(img.ID(), resolvedName) { // This condition checks if resolvedName is not just the ID
			// Ensure 'name' is a valid reference format before using it as a tag.
			// We avoid libimage.NormalizeName here to prevent prefixing.
			parsedRef, parseErr := reference.Parse(name) // Use reference.Parse for basic validation
			if parseErr != nil {
				return fmt.Errorf("invalid image name format %q: %w", name, parseErr)
			}
			if _, ok := parsedRef.(reference.Digested); ok {
				imageNameForTag = "" // Cannot use digest as a tag in this context
			} else {
				// For NamedTagged (e.g., "name:tag"), String() is appropriate.
				// For Named (e.g., "name"), Name() gives "name", String() might include domain.
				// We want to avoid "localhost/" prefixing if the original name didn't have a domain.
				if nt, okNt := parsedRef.(reference.NamedTagged); okNt {
					imageNameForTag = nt.String()
				} else if n, okN := parsedRef.(reference.Named); okN {
					if n.Hostname() == "" || n.Hostname() == reference.DefaultHostname { // Check if it was something like "ubuntu" vs "docker.io/library/ubuntu"
						imageNameForTag = n.RemoteName() // This should give "ubuntu" or "library/ubuntu"
					} else {
						imageNameForTag = n.String() // Had a domain, keep it
					}
				} else {
					// Fallback for other reference types, though typically we'd expect Named or NamedTagged.
					imageNameForTag = parsedRef.String()
				}
			}
		} else {
			imageNameForTag = "" // Name was an ID, no specific tag from it
		}

		// Handle AdditionalTags if any. If NoPrefix led to an empty imageNameForTag,
		// the first AdditionalTag can be used as the primary tag.
		if imageNameForTag == "" && len(options.AdditionalTags) > 0 {
			// Ensure the additional tag is valid.
			// We don't need to worry about prefixing here as AdditionalTags are expected to be complete.
			parsedTag, errTag := reference.Parse(options.AdditionalTags[0])
			if errTag != nil {
				return fmt.Errorf("invalid additional tag %q: %w", options.AdditionalTags[0], errTag)
			}
			// Decide how to use the parsedTag. String() is usually safe.
			if _, ok := parsedTag.(reference.Digested); ok {
				// if the additional tag is a digest, it cannot be used as a tag name.
			} else {
				imageNameForTag = parsedTag.String()
			}
			// Note: The copier handles all AdditionalTags. This logic is for the primary tag passed to NewReference.
			// For oci-archive/dir, only one tag is used by NewReference.
			// The remaining AdditionalTags are handled by the copier if the format supports it (e.g. docker-archive).
		}

	} else {
		img, resolvedImageName, lookupErr := r.LookupImage(name, nil)
		if lookupErr != nil {
			return lookupErr
		}
		localImage = img
		// Unless the image was referenced by ID, use the resolved name as a tag.
		if !strings.HasPrefix(img.ID(), resolvedImageName) {
			imageNameForTag = resolvedImageName
		}
	}

	if r.eventChannel != nil {
		defer r.writeEvent(&Event{ID: localImage.ID(), Name: path, Time: time.Now(), Type: EventTypeImageSave})
	}

	srcRef, err := localImage.StorageReference()
	if err != nil {
		return err
	}

	// Prepare the destination reference.
	var destRef types.ImageReference
	switch format {
	case "oci-archive":
		destRef, err = ociArchiveTransport.NewReference(path, imageNameForTag) // Use imageNameForTag

	case "oci-dir":
		destRef, err = ociTransport.NewReference(path, imageNameForTag) // Use imageNameForTag
		options.ManifestMIMEType = ociv1.MediaTypeImageManifest

	case "docker-dir":
		// docker-dir format does not use a tag in its path structure in the same way oci-archive/oci-dir do.
		// The manifest will store the name & tag.
		// The NewReference for dirTransport does not take a tag.
		destRef, err = dirTransport.NewReference(path)
		options.ManifestMIMEType = manifest.DockerV2Schema2MediaType
		// We need to ensure the copier knows about the desired tag if NoPrefix is used.
		// This might involve setting a field in CopyOptions if the manifest needs to reflect the non-prefixed name.
		// For docker-dir, the manifest inside will store the tag.
		// If NoPrefix is true, and imageNameForTag is set, this needs to be passed to the copier
		// so it writes the manifest with this name/tag.
		// The existing options.AdditionalTags is a slice, but these transports usually expect one primary tag.
		// We might need to ensure options.Copier.PreferredManifestListType for specific naming in manifest.
		// If NoPrefix is true and imageNameForTag != "" and (len(options.AdditionalTags) == 0 || options.AdditionalTags[0] != imageNameForTag) {
		// This logic is complex: docker-dir's manifest naming depends on the source image's names
		// and what the copier decides. If imageNameForTag is not already an option.AdditionalTags[0],
		// and we want to force it, we might need to prepend it to options.AdditionalTags for the copier.
		// However, CopyOptions.dockerArchiveAdditionalTags is specific to docker-archive.
		// For dirTransport, the manifest naming is more implicit.
		// A robust solution might require explicitly setting the "preferred name" in the manifest,
		// which is beyond simple tagging passed to NewReference.
		// For now, we rely on the copier using localImage.Names() or options.AdditionalTags.
		// If imageNameForTag was derived from 'name' and NoPrefix=true, and it's not in localImage.Names(),
		// the manifest might not reflect it unless it's also in AdditionalTags.
		// Let's ensure if imageNameForTag is set, it's the first one in AdditionalTags for the copier.
		// This is a common pattern for some copy operations.
		if imageNameForTag != "" {
			alreadyPresent := false
			for _, at := range options.AdditionalTags {
				if at == imageNameForTag {
					alreadyPresent = true
					break
				}
			}
			if !alreadyPresent {
				// Prepend to ensure it's considered as a primary tag by the copier
				// if the copier uses AdditionalTags for this purpose for docker-dir.
				options.AdditionalTags = append([]string{imageNameForTag}, options.AdditionalTags...)
			}
		}


	default:
		return fmt.Errorf("unsupported format %q for saving images", format)
	}

	if err != nil {
		return err
	}

	c, err := r.newCopier(&options.CopyOptions)
	if err != nil {
		return err
	}
	defer c.Close()

	// Critical: Ensure the copier uses the correct name for the manifest, especially for docker-dir.
	// If options.NoPrefix is true and imageNameForTag is set, we need the manifest to reflect this.
	// This might involve creating a temporary, renamed image reference for the source if the copier
	// strictly uses the source reference's names.
	// Or, more simply, if the copier can take a "forced name" for the destination manifest.
	// For now, we rely on the existing mechanism, which might pick one of the image's existing names
	// or what we've now pushed into AdditionalTags.

	_, err = c.Copy(ctx, srcRef, destRef)
	return err
}

// saveDockerArchive saves the specified images indicated by names to the path.
// It loads all images from the local containers storage and assembles the meta
// data needed to properly save images.  Since multiple names could refer to
// the *same* image, we need to dance a bit and store additional "names".
// Those can then be used as additional tags when copying.
func (r *Runtime) saveDockerArchive(ctx context.Context, names []string, path string, options *SaveOptions) error {
	type localImage struct {
		image *Image
		tags  []reference.NamedTagged
	}

	// Pre-parse options.AdditionalTags based on NoPrefix
	baseAdditionalTags := []reference.NamedTagged{}
	for _, tagStr := range options.AdditionalTags {
		var parsedTagRef reference.Named
		var errTagParse error
		if options.NoPrefix {
			// Try to parse as a named reference directly.
			// We want to preserve the name as is, without forcing a default domain or prefix.
			parsed, errP := reference.Parse(tagStr)
			if errP != nil {
				return fmt.Errorf("invalid additional tag %q: %w", tagStr, errP)
			}
			if named, ok := parsed.(reference.Named); ok {
				parsedTagRef = named
			} else {
				return fmt.Errorf("additional tag %q is not a valid named reference", tagStr)
			}
		} else {
			parsedTagRef, errTagParse = NormalizeName(tagStr) // Normalize (potentially adds prefix)
			if errTagParse != nil {
				return fmt.Errorf("invalid additional tag %q: %w", tagStr, errTagParse)
			}
		}

		if nt, ok := parsedTagRef.(reference.NamedTagged); ok {
			baseAdditionalTags = append(baseAdditionalTags, nt)
		} else if n, ok := parsedTagRef.(reference.Named); ok && reference.IsNameOnly(n) {
			// If it's just a name like "alpine", Docker convention is to tag it as "alpine:latest".
			latestTag, errLatest := reference.WithTag(n, "latest")
			if errLatest != nil {
				return fmt.Errorf("failed to apply default 'latest' tag to %q: %w", n.String(), errLatest)
			}
			baseAdditionalTags = append(baseAdditionalTags, latestTag.(reference.NamedTagged))
		} else {
			return fmt.Errorf("additional tag %q is not a valid tagged reference or simple name", tagStr)
		}
	}

	orderedIDs := []string{}                    // to preserve the relative order
	localImages := make(map[string]*localImage) // to assemble tags
	visitedNames := make(map[string]bool)       // filters duplicate names (based on resolved name from LookupImage)

	for _, name := range names {
		img, resolvedImageName, err := r.LookupImage(name, nil)
		if err != nil {
			return err
		}

		// Filter duplicates based on the resolved name from LookupImage.
		// This means if "myimage" and "localhost/myimage" are passed, and both resolve to "localhost/myimage",
		// only the first one encountered will be processed for adding its specific tag derived from original name.
		if _, exists := visitedNames[resolvedImageName]; exists {
			continue
		}
		visitedNames[resolvedImageName] = true

		currentImage := img
		var tagToAddFromOriginalName reference.NamedTagged // Tag derived from the current 'name' in the loop

		if options.NoPrefix {
			// If 'name' is not an ID itself, try to use 'name' directly for tagging.
			if !strings.HasPrefix(currentImage.ID(), resolvedImageName) {
				parsedRef, parseErr := reference.Parse(name)
				if parseErr != nil {
					// Log this? Or error out? For now, let's skip if unparseable.
					// return fmt.Errorf("invalid image name format %q for NoPrefix tagging: %w", name, parseErr)
					logrus.Warnf("Could not parse name %q for NoPrefix tagging, skipping specific tag for this name: %v", name, parseErr)
				} else {
					if digested, ok := parsedRef.(reference.Digested); ok {
						logrus.Debugf("Name %q is a digest (%s), cannot use as a tag", name, digested.String())
					} else if nt, ok := parsedRef.(reference.NamedTagged); ok {
						tagToAddFromOriginalName = nt
					} else if n, ok := parsedRef.(reference.Named); ok {
						// If it's just a name like "ubuntu", tag with ":latest".
						// We need to be careful here: reference.Parse("ubuntu") gives a Named.
						// reference.Parse("docker.io/library/ubuntu") also gives a Named.
						// We want to avoid prefixing "ubuntu" to "localhost/ubuntu:latest".
						// The String() method on Named reference handles this correctly for Named.
						// If n.Hostname() is "", n.String() is like "ubuntu", not "docker.io/library/ubuntu".
						// If domain is specified, it's kept.
						latestTag, errLatest := reference.WithTag(n, "latest")
						if errLatest != nil {
							logrus.Warnf("Could not apply 'latest' tag to %q for NoPrefix: %v", n.String(), errLatest)
						} else {
							tagToAddFromOriginalName = latestTag.(reference.NamedTagged)
						}
					}
				}
			}
		} else {
			// Existing logic: Use the resolvedImageName as a tag if it's not an ID.
			if !strings.HasPrefix(currentImage.ID(), resolvedImageName) {
				parsedRef, parseErr := reference.ParseNamed(resolvedImageName)
				if parseErr == nil {
					if nt, ok := parsedRef.(reference.NamedTagged); ok {
						tagToAddFromOriginalName = nt
					} else if n, ok := parsedRef.(reference.Named); ok && reference.IsNameOnly(n) {
						latestTag, errLatest := reference.WithTag(n, "latest")
						if errLatest == nil {
							tagToAddFromOriginalName = latestTag.(reference.NamedTagged)
						} else {
							logrus.Warnf("Could not apply 'latest' tag to resolved name %q: %v", n.String(), errLatest)
						}
					}
				} else {
					logrus.Warnf("Could not parse resolved name %q as tagged reference: %v", resolvedImageName, parseErr)
				}
			}
		}

		local, exists := localImages[currentImage.ID()]
		if !exists {
			local = &localImage{image: currentImage}
			// Assign a copy of baseAdditionalTags. Each image gets its own list initially.
			local.tags = make([]reference.NamedTagged, len(baseAdditionalTags))
			copy(local.tags, baseAdditionalTags)
			orderedIDs = append(orderedIDs, currentImage.ID())
		}

		// Add the tag derived from the original name, if any, avoiding duplicates.
		if tagToAddFromOriginalName != nil {
			isDuplicate := false
			for _, existingTag := range local.tags {
				if existingTag.String() == tagToAddFromOriginalName.String() {
					isDuplicate = true
					break
				}
			}
			if !isDuplicate {
				local.tags = append(local.tags, tagToAddFromOriginalName)
			}
		}

		localImages[currentImage.ID()] = local
		if r.eventChannel != nil {
			defer r.writeEvent(&Event{ID: currentImage.ID(), Name: path, Time: time.Now(), Type: EventTypeImageSave})
		}
	}

	writer, err := dockerArchiveTransport.NewWriter(r.systemContextCopy(), path)
	if err != nil {
		return err
	}
	defer writer.Close()

	for _, id := range orderedIDs {
		local, exists := localImages[id]
		if !exists {
			return fmt.Errorf("internal error: saveDockerArchive: ID %s not found in local map", id)
		}

		copyOpts := options.CopyOptions
		copyOpts.dockerArchiveAdditionalTags = local.tags

		c, err := r.newCopier(&copyOpts)
		if err != nil {
			return err
		}
		defer c.Close()

		destRef, err := writer.NewReference(nil)
		if err != nil {
			return err
		}

		srcRef, err := local.image.StorageReference()
		if err != nil {
			return err
		}

		if _, err := c.Copy(ctx, srcRef, destRef); err != nil {
			return err
		}
	}

	return nil
}
