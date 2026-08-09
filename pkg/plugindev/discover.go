package plugindev

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// exeExt returns the executable suffix for the current platform.
func exeExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// ListPlugins discovers decoder plugins laid out under root.
//
// Plugins may live either as a bare top-level executable (<root>/<name>.exe on
// Windows, <root>/<name> elsewhere — legacy layout) or, as produced by
// Scaffold, as a subdirectory (<root>/<name>/<name>.exe). Subdirectories are
// scanned one level deep; the directory name is the plugin name. When a
// subdirectory holds multiple executables they collapse to a single entry,
// preferring <name>/<name>.exe, otherwise the first executable alphabetically.
//
// A missing root is not an error: it yields an empty list (the plugin surface
// is simply empty before anything is scaffolded).
func ListPlugins(root string) ([]*DiscoveredPlugin, error) {
	info, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("plugins path is not a directory: %s", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	ext := exeExt()
	found := make(map[string]*DiscoveredPlugin)
	for _, e := range entries {
		if e.IsDir() {
			name := e.Name()
			sub, err := os.ReadDir(filepath.Join(root, name))
			if err != nil {
				continue
			}
			var binary string
			for _, f := range sub {
				if f.IsDir() {
					continue
				}
				fname := f.Name()
				if runtime.GOOS == "windows" && !strings.HasSuffix(fname, ext) {
					continue
				}
				p := filepath.Join(root, name, fname)
				if fname == name+ext {
					binary = p
					break
				}
				if binary == "" {
					binary = p
				}
			}
			if binary != "" {
				found[name] = &DiscoveredPlugin{Name: name, Binary: binary, Dir: filepath.Join(root, name)}
			}
			continue
		}

		// Top-level (legacy) executable.
		if runtime.GOOS == "windows" {
			if !strings.HasSuffix(e.Name(), ext) {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ext)
			found[name] = &DiscoveredPlugin{Name: name, Binary: filepath.Join(root, e.Name()), Dir: root}
		} else {
			found[e.Name()] = &DiscoveredPlugin{Name: e.Name(), Binary: filepath.Join(root, e.Name()), Dir: root}
		}
	}

	result := make([]*DiscoveredPlugin, 0, len(found))
	for _, p := range found {
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}
