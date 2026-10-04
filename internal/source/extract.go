package source

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Stats summarizes one extraction.
type Stats struct {
	ArchiveBytes int64 // compressed bytes read
	Files        int
	Directories  int
	Symlinks     int
	Bytes        int64 // regular-file bytes written
}

var errArchiveTooLarge = errors.New("archive exceeds the compressed size limit")

// boundedReader stops the stream once more than limit bytes were read and
// observes cancellation between reads.
type boundedReader struct {
	ctx   context.Context
	r     io.Reader
	limit int64
	n     int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := b.r.Read(p)
	b.n += int64(n)
	if b.n > b.limit {
		return n, errArchiveTooLarge
	}
	return n, err
}

type pendingLink struct{ name, target string }

type extractor struct {
	root    *os.Root
	limits  Limits
	prefix  string
	files   map[string]bool // regular files and symlinks
	dirs    map[string]bool // explicit and implied directories
	links   []pendingLink
	stats   Stats
	entries int
}

// extract streams a gzip-compressed tar archive into root.
//
// The archive must begin with git's global header declaring exactly wantSHA
// (git archive records the commit as the "comment" PAX record), contain a single
// top-level directory, and hold only directories, regular files and relative
// symlinks that stay inside the tree. Paths must be canonical and local; duplicate
// or overlapping entries are rejected. Symlinks are created only after every file
// and directory exists, so no write can pass through one, and every write goes
// through os.Root, which refuses to escape the tree. Readers of the tree must keep
// using os.Root (or an equivalent) because a link may still point at another link.
func extract(ctx context.Context, archive io.Reader, root *os.Root, wantSHA string, limits Limits) (Stats, error) {
	compressed := &boundedReader{ctx: ctx, r: archive, limit: limits.MaxArchiveBytes}
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return Stats{}, readFailure(ctx, err)
	}
	defer func() { _ = gz.Close() }()
	x := &extractor{root: root, limits: limits, files: map[string]bool{}, dirs: map[string]bool{".": true}}
	tr := tar.NewReader(gz)
	identified := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, tar.ErrInsecurePath) {
			return x.stats, failure(CodeUnsafeArchive, "The archive contains a path that escapes the source tree.", nil)
		}
		if err != nil {
			return x.stats, readFailure(ctx, err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			if identified || x.entries > 0 || x.prefix != "" {
				return x.stats, failure(CodeUnsafeArchive, "The archive contains an unexpected global header.", nil)
			}
			if hdr.PAXRecords["comment"] != wantSHA {
				return x.stats, failure(CodeIdentityMismatch, "The archive does not contain the requested commit.", nil)
			}
			identified = true
			continue
		}
		if !identified {
			return x.stats, failure(CodeIdentityMismatch, "The archive does not declare which commit it contains.", nil)
		}
		if err := x.entry(tr, hdr); err != nil {
			return x.stats, err
		}
	}
	// Decompressors buffer ahead and may swallow the limit error when the whole
	// stream arrived in one read, so check the bytes actually consumed.
	if compressed.n > limits.MaxArchiveBytes {
		return x.stats, failure(CodeTooLarge, "The source archive exceeds the download size limit.", nil)
	}
	if !identified {
		return x.stats, failure(CodeIdentityMismatch, "The archive does not declare which commit it contains.", nil)
	}
	if x.entries == 0 {
		return x.stats, failure(CodeInvalidArchive, "The archive contains no files.", nil)
	}
	if err := x.createLinks(); err != nil {
		return x.stats, err
	}
	x.stats.ArchiveBytes = compressed.n
	return x.stats, nil
}

func (x *extractor) entry(tr io.Reader, hdr *tar.Header) error {
	name, isRoot, err := x.relative(hdr.Name)
	if err != nil {
		return err
	}
	if isRoot {
		if hdr.Typeflag != tar.TypeDir {
			return failure(CodeUnsafeArchive, "The archive's top-level entry is not a directory.", nil)
		}
		return nil
	}
	x.entries++
	if x.entries > x.limits.MaxEntries {
		return failure(CodeTooManyFiles, fmt.Sprintf("The source contains more than %d entries.", x.limits.MaxEntries), nil)
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if x.files[name] {
			return overlapping()
		}
		if err := x.markParents(name); err != nil {
			return err
		}
		x.dirs[name] = true
		if err := x.root.MkdirAll(name, 0o755); err != nil {
			return writeFailure(err)
		}
		x.stats.Directories++
	case tar.TypeReg:
		if x.files[name] || x.dirs[name] {
			return overlapping()
		}
		if hdr.Size < 0 || hdr.Size > x.limits.MaxExtractedBytes-x.stats.Bytes {
			return failure(CodeTooLarge, fmt.Sprintf("The extracted source would exceed %d bytes.", x.limits.MaxExtractedBytes), nil)
		}
		if err := x.markParents(name); err != nil {
			return err
		}
		x.files[name] = true
		if parent := path.Dir(name); parent != "." {
			if err := x.root.MkdirAll(parent, 0o755); err != nil {
				return writeFailure(err)
			}
		}
		perm := os.FileMode(0o644)
		if hdr.Mode&0o111 != 0 {
			perm = 0o755 // Keep the executable bit only; never setuid, setgid or sticky.
		}
		f, err := x.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return writeFailure(err)
		}
		written, copyErr := io.Copy(f, io.LimitReader(tr, hdr.Size))
		closeErr := f.Close()
		if copyErr != nil {
			return readFailure(context.Background(), copyErr)
		}
		if written != hdr.Size {
			return retryable(CodeInvalidArchive, "The archive ended before a file was complete.", nil)
		}
		if closeErr != nil {
			return writeFailure(closeErr)
		}
		x.stats.Files++
		x.stats.Bytes += written
	case tar.TypeSymlink:
		if err := x.validateLink(name, hdr.Linkname); err != nil {
			return err
		}
		if x.files[name] || x.dirs[name] {
			return overlapping()
		}
		if err := x.markParents(name); err != nil {
			return err
		}
		x.files[name] = true
		x.links = append(x.links, pendingLink{name: name, target: hdr.Linkname})
		x.stats.Symlinks++
	default:
		return failure(CodeUnsafeArchive, "The archive contains a hard link, device, FIFO or other unsupported entry.", nil)
	}
	return nil
}

// relative strips the single top-level directory and validates the remainder.
func (x *extractor) relative(raw string) (string, bool, error) {
	if raw == "" || strings.ContainsRune(raw, 0) {
		return "", false, failure(CodeUnsafeArchive, "The archive contains an invalid path.", nil)
	}
	top, rest, _ := strings.Cut(strings.TrimSuffix(raw, "/"), "/")
	if top == "" || top == "." || top == ".." {
		return "", false, failure(CodeUnsafeArchive, "The archive contains a path that escapes the source tree.", nil)
	}
	if x.prefix == "" {
		x.prefix = top
	} else if top != x.prefix {
		return "", false, failure(CodeUnsafeArchive, "The archive has more than one top-level directory.", nil)
	}
	if rest == "" {
		return "", true, nil
	}
	if len(rest) > x.limits.MaxPathBytes {
		return "", false, failure(CodeUnsafeArchive, "The archive contains a path longer than the supported limit.", nil)
	}
	if !filepath.IsLocal(rest) || path.Clean(rest) != rest {
		return "", false, failure(CodeUnsafeArchive, "The archive contains a path that escapes the source tree.", nil)
	}
	for _, component := range strings.Split(rest, "/") {
		if len(component) > 255 {
			return "", false, failure(CodeUnsafeArchive, "The archive contains a file name longer than 255 bytes.", nil)
		}
	}
	return rest, false, nil
}

// markParents records implied directories and rejects entries beneath a file or link.
func (x *extractor) markParents(name string) error {
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		if x.files[parent] {
			return overlapping()
		}
		if x.dirs[parent] {
			return nil // Its own parents were recorded when it was added.
		}
		x.dirs[parent] = true
	}
	return nil
}

func (x *extractor) validateLink(name, target string) error {
	if target == "" || len(target) > x.limits.MaxPathBytes || strings.ContainsRune(target, 0) || strings.HasPrefix(target, "/") || filepath.IsAbs(target) {
		return failure(CodeUnsafeArchive, "The archive contains a symlink with an absolute or invalid target.", nil)
	}
	if !filepath.IsLocal(path.Join(path.Dir(name), target)) {
		return failure(CodeUnsafeArchive, "The archive contains a symlink that points outside the source tree.", nil)
	}
	return nil
}

func (x *extractor) createLinks() error {
	for _, link := range x.links {
		if parent := path.Dir(link.name); parent != "." {
			if err := x.root.MkdirAll(parent, 0o755); err != nil {
				return writeFailure(err)
			}
		}
		if err := x.root.Symlink(link.target, link.name); err != nil {
			return writeFailure(err)
		}
	}
	return nil
}

func overlapping() error {
	return failure(CodeUnsafeArchive, "The archive contains duplicate or overlapping entries.", nil)
}

func readFailure(ctx context.Context, err error) error {
	switch {
	case errors.Is(err, errArchiveTooLarge):
		return failure(CodeTooLarge, "The source archive exceeds the download size limit.", nil)
	case ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		cause := ctx.Err()
		if cause == nil {
			cause = err
		}
		return &Error{Code: CodeCancelled, Message: "Source preparation was cancelled or timed out.", Retryable: errors.Is(cause, context.DeadlineExceeded), cause: cause}
	case errors.Is(err, io.ErrUnexpectedEOF):
		return retryable(CodeInvalidArchive, "The archive download ended early.", nil)
	case errors.Is(err, gzip.ErrHeader), errors.Is(err, gzip.ErrChecksum), errors.Is(err, tar.ErrHeader), errors.Is(err, tar.ErrFieldTooLong), errors.Is(err, tar.ErrWriteTooLong):
		return failure(CodeInvalidArchive, "The source archive is malformed.", nil)
	default:
		// Network errors can embed request URLs; keep only the classification.
		return retryable(CodeAPIUnavailable, "The source archive download was interrupted.", nil)
	}
}

func writeFailure(err error) error {
	if errors.Is(err, os.ErrExist) {
		return overlapping()
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) && strings.Contains(pathErr.Err.Error(), "escapes") {
		return failure(CodeUnsafeArchive, "The archive tried to write outside the source tree.", nil)
	}
	return retryable(CodeWorkspaceFailure, "The worker could not write the source tree.", err)
}
