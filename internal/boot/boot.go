// Package boot resolves what to boot — the kernel plus, when a bundle supplies
// them, a matching modules tree and device tree — from the arch defaults and the
// -kernel/-bundle overrides. URL kernels/bundles are downloaded once and cached;
// a bundleConfig is built locally with the bundler and cached by config hash.
package boot

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/samuel/qemu-test/internal/arch"
	"github.com/samuel/qemu-test/internal/bundler"
	"github.com/samuel/qemu-test/internal/cache"
	"github.com/samuel/qemu-test/internal/modules"
)

// Env is the resolved set of host artifacts needed to boot a guest: the kernel
// plus, when a bundle supplies them, a matching modules tree and a device tree.
type Env struct {
	KernelPath string
	ModulesDir string // path to the modules tree (contains modules.dep), or "" if none
	DTBPath    string // device tree to pass via -dtb, or "" if none
}

// Options carries the inputs Resolve needs: the target arch, the -kernel/-bundle
// overrides, whether to re-validate URL sources (-refresh), and a work dir that
// receives a local tarball bundle's extracted tree.
type Options struct {
	Arch           arch.Arch
	KernelOverride string // -kernel path or URL (bare kernel)
	BundleOverride string // -bundle path or URL (boot bundle: kernel + modules + dtb)
	Refresh        bool   // re-validate a URL kernel or bundle against the remote
	WorkDir        string // temp dir to extract a local tarball bundle into
}

// Resolve resolves what to boot. Precedence: a -bundle override → a -kernel
// override (bare kernel) → the arch's BundleConfig (built locally) → the arch's
// BundleURL → the arch's KernelURL (bare kernel) → error. -bundle and -kernel are
// mutually exclusive (rejected earlier). URL kernels/bundles are downloaded once
// and cached; refresh re-validates them. o.WorkDir receives a local tarball
// bundle's extracted tree.
func Resolve(ctx context.Context, o Options) (Env, error) {
	a := o.Arch
	switch {
	case o.BundleOverride != "":
		return resolveBundle(ctx, a, o.BundleOverride, o.Refresh, o.WorkDir)
	case o.KernelOverride != "":
		kp, err := resolveBareKernel(ctx, o.KernelOverride, o.Refresh)
		return Env{KernelPath: kp}, err
	case a.BundleConfig != nil:
		return resolveBuiltBundle(ctx, a, o.Refresh, o.WorkDir)
	case a.BundleURL != "":
		return resolveBundle(ctx, a, a.BundleURL, o.Refresh, o.WorkDir)
	case a.KernelURL != "":
		kp, err := resolveBareKernel(ctx, a.KernelURL, o.Refresh)
		return Env{KernelPath: kp}, err
	default:
		return Env{}, fmt.Errorf("no kernel or bundle for arch %q; supply one with -kernel or -bundle <path-or-URL>", a.Name)
	}
}

// resolveBuiltBundle builds the arch's BundleConfig into a boot bundle with the
// bundler package, then resolves it exactly like a downloaded bundle. The bundle is
// deterministic, so it is cached by a hash of its config (a config change rebuilds)
// to skip the build's heavy work — fetching the upstream sources and selecting files
// out of them — on later runs; the bundler caches the source downloads themselves.
// -refresh re-validates those sources against the remote and rebuilds.
func resolveBuiltBundle(ctx context.Context, a arch.Arch, refresh bool, workDir string) (Env, error) {
	cacheDir, err := cache.CacheDir()
	if err != nil {
		return Env{}, err
	}
	cfgJSON, err := json.Marshal(a.BundleConfig)
	if err != nil {
		return Env{}, fmt.Errorf("encode bundle config for arch %q: %w", a.Name, err)
	}
	builtPath := filepath.Join(cacheDir, cache.CacheKey(string(cfgJSON))+cache.BuiltBundleExt)
	if _, err := os.Stat(builtPath); err != nil || refresh {
		if err := bundler.Bundle(ctx, a.BundleConfig, builtPath, cacheDir, refresh); err != nil {
			return Env{}, fmt.Errorf("build bundle for arch %q: %w", a.Name, err)
		}
	} else {
		slog.Debug("using cached built bundle", "arch", a.Name, "path", builtPath)
	}
	// From here the built bundle is used exactly like a downloaded one: extracted
	// into the work dir and resolved into its kernel/modules/dtb paths.
	return resolveBundle(ctx, a, builtPath, refresh, workDir)
}

// resolveBareKernel resolves a single kernel source (a local path or an http(s)
// URL) to a host path, fetching + caching URL kernels.
func resolveBareKernel(ctx context.Context, src string, refresh bool) (string, error) {
	if !cache.IsHTTPURL(src) {
		path, err := filepath.Abs(src)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("kernel %q: %w", src, err)
		}
		return path, nil
	}
	cacheDir, err := cache.CacheDir()
	if err != nil {
		return "", err
	}
	return cache.FetchKernel(ctx, nil, src, cacheDir, refresh)
}

// resolveBundle resolves a boot bundle from src — an http(s) URL (fetched, cached,
// and extracted), a local tarball (extracted into workDir), or a local directory
// (used in place) — into the kernel/modules/dtb paths it carries.
func resolveBundle(ctx context.Context, a arch.Arch, src string, refresh bool, workDir string) (Env, error) {
	var dir string
	if cache.IsHTTPURL(src) {
		cacheDir, err := cache.CacheDir()
		if err != nil {
			return Env{}, err
		}
		dir, err = cache.FetchBundle(ctx, nil, src, cacheDir, refresh)
		if err != nil {
			return Env{}, err
		}
	} else {
		abs, err := filepath.Abs(src)
		if err != nil {
			return Env{}, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return Env{}, fmt.Errorf("bundle %q: %w", src, err)
		}
		if info.IsDir() {
			dir = abs
		} else {
			dir = filepath.Join(workDir, "bundle")
			if err := cache.ExtractArchiveAtomic(abs, dir, workDir); err != nil {
				return Env{}, fmt.Errorf("extract bundle %q: %w", src, err)
			}
		}
	}
	return bundleEnv(a, dir)
}

// bundleEnv resolves the kernel/modules/dtb paths inside an extracted bundle
// directory, defaulting to the layout convention and honoring any arch overrides.
func bundleEnv(a arch.Arch, dir string) (Env, error) {
	kernelRel := a.BundleKernelPath
	if kernelRel == "" {
		kernelRel = "vmlinuz"
	}
	kernelPath := filepath.Join(dir, kernelRel)
	if _, err := os.Stat(kernelPath); err != nil {
		return Env{}, fmt.Errorf("bundle kernel %q: %w", kernelRel, err)
	}
	modulesDir, err := bundleModulesDir(a, dir)
	if err != nil {
		return Env{}, err
	}
	var dtbPath string
	if a.BundleDTBPath != "" {
		dtbPath = filepath.Join(dir, a.BundleDTBPath)
		if _, err := os.Stat(dtbPath); err != nil {
			return Env{}, fmt.Errorf("bundle dtb %q: %w", a.BundleDTBPath, err)
		}
	}
	return Env{KernelPath: kernelPath, ModulesDir: modulesDir, DTBPath: dtbPath}, nil
}

// bundleModulesDir locates the modules tree inside a bundle: the arch override if
// set, else "modules/", else the sole "lib/modules/<release>/". A tree is detected
// by modules.dep or any .ko file (isModuleTree), so trees that ship only modules
// (no depmod output) still count. It returns "" (and no error) when the bundle
// carries no modules tree (a bare kernel; modules built in).
func bundleModulesDir(a arch.Arch, dir string) (string, error) {
	if a.BundleModulesPath != "" {
		md := filepath.Join(dir, a.BundleModulesPath)
		if isModuleTree(md) {
			return md, nil
		}
		return "", fmt.Errorf("bundle modules path %q is not a modules tree (no modules.dep or .ko files)", a.BundleModulesPath)
	}
	if md := filepath.Join(dir, "modules"); isModuleTree(md) {
		return md, nil
	}
	entries, err := os.ReadDir(filepath.Join(dir, "lib", "modules"))
	if err != nil {
		return "", nil // no lib/modules → no modules tree
	}
	var releases []string
	for _, e := range entries {
		if e.IsDir() && isModuleTree(filepath.Join(dir, "lib", "modules", e.Name())) {
			releases = append(releases, e.Name())
		}
	}
	switch len(releases) {
	case 0:
		return "", nil
	case 1:
		return filepath.Join(dir, "lib", "modules", releases[0]), nil
	default:
		return "", fmt.Errorf("bundle has multiple module trees under lib/modules (%s); set the arch BundleModulesPath", strings.Join(releases, ", "))
	}
}

// isModuleTree reports whether dir looks like a kernel modules tree: it has a
// modules.dep (text or .bin), or contains at least one .ko file (so a tree that
// ships only modules — e.g. an extracted netboot initrd — is recognized too).
func isModuleTree(dir string) bool {
	if modules.HasModulesDep(dir) {
		return true
	}
	var found bool
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // a walk error just means we didn't find a .ko here
		}
		if !d.IsDir() && modules.IsKoFile(d.Name()) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}
