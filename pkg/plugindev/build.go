package plugindev

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// goBuildErrRe matches a `go build` diagnostic line:
//
//	main.go:42:9: undefined: event.ValueInt32
var goBuildErrRe = regexp.MustCompile(`^(.+?):(\d+):(\d+):\s*(.+)$`)

// Build compiles the plugin project at Root/Name using `go build`, producing an
// executable next to the sources. Build errors are parsed into structured
// file:line:col entries so callers can pinpoint fixes without scraping raw
// output. A failed compile is a normal (OK=false) result, not a transport
// error.
func Build(ctx context.Context, req *BuildRequest) (*BuildResponse, error) {
	if req.Root == "" {
		return nil, fmt.Errorf("root (plugins dir) is required")
	}
	if req.Name == "" {
		return nil, fmt.Errorf("name is required")
	}

	dir := filepath.Join(req.Root, req.Name)
	timeout := req.TimeoutSec
	if timeout <= 0 {
		timeout = 120
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	outName := req.Name
	if runtime.GOOS == "windows" {
		outName += ".exe"
	}

	cmd := exec.CommandContext(ctx, "go", "build", "-o", outName, ".")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()

	resp := &BuildResponse{Output: string(out)}
	if err != nil {
		resp.Errors = parseGoBuildErrors(string(out))
		resp.OK = false
		return resp, nil
	}
	resp.OK = true
	return resp, nil
}

func parseGoBuildErrors(out string) []*BuildError {
	var errs []*BuildError
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := goBuildErrRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		be := &BuildError{File: m[1], Message: m[4]}
		if n, e := strconv.Atoi(m[2]); e == nil {
			be.Line = n
		}
		if n, e := strconv.Atoi(m[3]); e == nil {
			be.Col = n
		}
		errs = append(errs, be)
	}
	return errs
}
