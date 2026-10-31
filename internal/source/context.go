package source

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ajaykathwatee/previa/internal/repoconfig"
)

// prepareContext narrows the filesystem boundary before checking the Dockerfile
// and files GitHub archives leave incomplete. The root config remains outside
// the context when a monorepo chooses a subfolder.
func prepareContext(ctx context.Context, root *os.Root, cfg repoconfig.Config) (string, error) {
	selected, err := root.OpenRoot(cfg.Context)
	if err != nil {
		return "", failure(CodeInvalidConfig, "The context in .previewrc.yaml must be a directory inside the repository.", nil)
	}
	defer func() { _ = selected.Close() }()
	resolved, err := filepath.EvalSymlinks(filepath.Join(root.Name(), cfg.Context))
	if err != nil {
		return "", failure(CodeInvalidConfig, "The build context could not be resolved.", err)
	}
	relative, err := filepath.Rel(root.Name(), resolved)
	if err != nil || !filepath.IsLocal(relative) {
		return "", failure(CodeInvalidConfig, "The build context must stay inside the repository.", err)
	}
	if err := checkSubmodulePaths(root, filepath.ToSlash(relative)); err != nil {
		return "", err
	}
	if err := checkSourceFeatures(ctx, selected); err != nil {
		return "", err
	}
	if err := checkDockerfile(selected, cfg.Dockerfile); err != nil {
		return "", err
	}
	return relative, nil
}

const submodulesMessage = "Git submodules are unsupported: GitHub's archive omits their contents. Vendor them into the build context or choose a context without submodules."

// The root manifest may name a submodule inside a selected subfolder. GitHub
// omits that directory, so walking the extracted context alone cannot find it.
func checkSubmodulePaths(root *os.Root, contextPath string) error {
	info, err := root.Lstat(".gitmodules")
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return writeFailure(err)
	}
	if contextPath == "." || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return failure(CodeSubmodules, submodulesMessage, nil)
	}
	f, err := root.OpenFile(".gitmodules", os.O_RDONLY|syscallNoFollow, 0)
	if err != nil {
		return failure(CodeSubmodules, submodulesMessage, nil)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || SubmodulesInContext(data, contextPath) {
		return failure(CodeSubmodules, submodulesMessage, nil)
	}
	return nil
}

// SubmodulesInContext reports whether a root .gitmodules manifest makes a
// build of contextPath (slash-separated, relative) fail: with the repository
// root as context any manifest does; otherwise a submodule path at, inside or
// around the context does. An oversized or malformed manifest counts as a
// conflict, as it does in a build. Setup checks apply the same rule.
func SubmodulesInContext(manifest []byte, contextPath string) bool {
	if contextPath == "." || len(manifest) > 64<<10 {
		return true
	}
	for _, line := range strings.Split(string(manifest), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(key) != "path" {
			continue
		}
		value = strings.TrimSpace(value)
		var err error
		if strings.HasPrefix(value, `"`) {
			value, err = strconv.Unquote(value)
		}
		if err != nil || !filepath.IsLocal(value) {
			return true
		}
		value = filepath.ToSlash(filepath.Clean(value))
		if value == contextPath || strings.HasPrefix(value, contextPath+"/") || strings.HasPrefix(contextPath, value+"/") {
			return true
		}
	}
	return false
}

var lfsPointer = regexp.MustCompile(`(?m)^oid sha256:[a-f0-9]{64}\r?$\nsize [0-9]+\r?$`)

// checkSourceFeatures reads only small regular files, without following links.
// Refuse incomplete contexts before they can build or publish placeholder data.
func checkSourceFeatures(ctx context.Context, root *os.Root) error {
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return readFailure(ctx, err)
		}
		if entry.Name() == ".gitmodules" {
			return failure(CodeSubmodules, submodulesMessage, nil)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 1024 {
			return err
		}
		return checkLFSPointer(root, name)
	})
	if err != nil && CodeOf(err) == "" {
		return retryable(CodeWorkspaceFailure, "The worker could not inspect the build context.", err)
	}
	return err
}

func checkLFSPointer(root *os.Root, name string) error {
	f, err := root.OpenFile(name, os.O_RDONLY|syscallNoFollow, 0)
	if err != nil {
		return fmt.Errorf("open source file: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil {
		return fmt.Errorf("read source file: %w", err)
	}
	if bytes.HasPrefix(data, []byte("version https://git-lfs.github.com/spec/v1\n")) || bytes.HasPrefix(data, []byte("version https://git-lfs.github.com/spec/v1\r\n")) {
		if lfsPointer.Match(data) {
			return failure(CodeLFS, "Git LFS pointer files are unsupported: this archive contains pointers instead of file contents. Include the real files in Git or choose a context without LFS pointers.", nil)
		}
	}
	return nil
}
