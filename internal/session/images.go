package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"minimalharness/internal/llm"
)

const maxImageBlobBytes = 10 << 20

func imageBlobDir() string { return filepath.Join(filepath.Dir(Dir()), "blobs", "sha256") }

func validImageDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	for _, c := range digest {
		if c < '0' || c > '9' && c < 'a' || c > 'f' {
			return false
		}
	}
	return true
}

func readImageBlob(root *os.Root, digest string) ([]byte, error) {
	if !validImageDigest(digest) {
		return nil, fmt.Errorf("invalid image digest")
	}
	before, err := root.Lstat(digest)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("image blob is not a regular file")
	}
	f, err := root.OpenFile(digest, os.O_RDONLY|imageBlobReadFlags, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) || info.Size() > maxImageBlobBytes {
		return nil, fmt.Errorf("invalid image blob size or type")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxImageBlobBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageBlobBytes {
		return nil, fmt.Errorf("image blob exceeds size cap")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, fmt.Errorf("image blob checksum mismatch")
	}
	return data, nil
}

func imageReference(img llm.Image) (llm.Image, []byte, error) {
	if img.Data == "" {
		return img, nil, nil
	} // Preserve unavailable and legacy references.
	if len(img.Data) > base64.StdEncoding.EncodedLen(maxImageBlobBytes) {
		return img, nil, fmt.Errorf("image exceeds size cap")
	}
	data, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil || len(data) > maxImageBlobBytes {
		return img, nil, fmt.Errorf("invalid or oversized image data")
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if img.Digest != "" && img.Digest != digest {
		return img, nil, fmt.Errorf("image bytes do not match their digest")
	}
	img.Digest = digest
	img.Bytes = len(data)
	img.Unavailable = ""
	return img, data, nil
}

// Save bytes before a transcript can refer to them. Concurrent publishers of
// one digest publish identical complete bytes.
func persistImage(img llm.Image) (llm.Image, error) {
	img, data, err := imageReference(img)
	if err != nil || img.Data == "" {
		return img, err
	}
	digest := img.Digest
	dir := imageBlobDir()
	base := filepath.Dir(Dir())
	for _, parent := range []string{base, filepath.Dir(dir), dir} {
		if err := os.Mkdir(parent, 0700); err != nil && !os.IsExist(err) {
			return img, err
		}
		// Also make newly-created directory entries durable, including entries
		// created concurrently by another publisher before its own sync.
		if err := syncRecoveryDirectory(filepath.Dir(parent)); err != nil {
			return img, err
		}
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return img, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return img, err
	}
	defer root.Close()
	if _, err := readImageBlob(root, digest); err == nil {
		if err := root.Chmod(digest, 0600); err != nil {
			return img, err
		}
	} else if !os.IsNotExist(err) {
		return img, err
	} else {
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return img, err
		}
		name := ".image-" + hex.EncodeToString(nonce[:])
		f, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return img, err
		}
		defer root.Remove(name)
		_, writeErr := f.Write(data)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil {
			return img, writeErr
		}
		if closeErr != nil {
			return img, closeErr
		}
		if err := root.Rename(name, digest); err != nil {
			return img, err
		}
	}
	// Sync even when deduplicating: another writer may just have published it.
	if err := syncRecoveryDirectory(dir); err != nil {
		return img, err
	}
	img.Digest = digest
	img.Bytes = len(data)
	img.Unavailable = ""
	return img, nil
}

func persistImages(m llm.Message) (llm.Message, error) {
	m.Images = slices.Clone(m.Images)
	for i, img := range m.Images {
		var err error
		m.Images[i], err = persistImage(img)
		if err != nil {
			return m, fmt.Errorf("save image %q: %w", img.Path, err)
		}
	}
	return m, nil
}

func persistMessageImages(msgs []llm.Message) ([]llm.Message, error) {
	out := slices.Clone(msgs)
	for i, m := range out {
		var err error
		out[i], err = persistImages(m)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// HydrateImages restores only immutable blobs. Origin paths are provenance,
// never a fallback: legacy path-only records cannot establish historical bytes.
func HydrateImages(msgs []llm.Message) {
	for i := range msgs {
		for j := range msgs[i].Images {
			img := &msgs[i].Images[j]
			if img.Data != "" {
				continue
			}
			if img.Digest == "" {
				img.Unavailable = "legacy attachment has no saved blob"
				continue
			}
			root, err := os.OpenRoot(imageBlobDir())
			if err != nil {
				img.Unavailable = "saved image blob is missing or unreadable"
				continue
			}
			data, err := readImageBlob(root, img.Digest)
			root.Close()
			if err != nil {
				img.Unavailable = "saved image blob is missing, unreadable or corrupt"
				continue
			}
			img.Data = base64.StdEncoding.EncodeToString(data)
			img.Bytes = len(data)
			img.Unavailable = ""
		}
	}
}

// ImageReferences computes the metadata that persistence records, without IO or
// changing caller-owned slices. Used to compare transient and persisted views.
func ImageReferences(msgs []llm.Message) ([]llm.Message, error) {
	out := slices.Clone(msgs)
	for i := range out {
		out[i].Images = slices.Clone(out[i].Images)
		for j, img := range out[i].Images {
			ref, _, err := imageReference(img)
			if err != nil {
				return nil, err
			}
			out[i].Images[j] = ref
		}
	}
	return out, nil
}
