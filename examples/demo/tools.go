// Package codetools provides the minimal viable tool set for a self-coding
// agent: read, search, write, edit, verify and version-control operations
// over a workspace rooted at one directory.
//
// The design assumes the workspace IS the agent's own source. Every
// mutating tool is RiskConfirm; approval is the only guardrail. The scope
// set (code:read, code:write, code:exec, code:vcs) is what an operator must
// grant for the tools to be visible at all, because the step's narrowed
// principal is the intersection of the caller's scopes and the exposed
// tools' scopes (see agent/step.go, selectTools).
//
// Deployment is intentionally NOT a tool. It is an effect dispatched by a
// supervisor outside this package, keyed per committed block, so a crashed
// run converges on its receipt instead of restarting itself in a loop.

// This package targets Unix. The shell tool shells out to sh and uses
// process groups (syscall.Setpgid) so a backgrounded grandchild cannot
// outlive its command; neither is available on Windows. The other tools
// (build, test, git, file operations) are portable, but the package as a
// whole does not compile on Windows because of the shell tool. If Windows
// is ever required, split the shell tool behind //go:build unix and add a
// cmd /c variant.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/asaidimu/workspace/agent"
	"github.com/asaidimu/workspace/kernel"
)

// Scopes.
const (
	ScopeRead  = "code:read"
	ScopeWrite = "code:write"
	ScopeExec  = "code:exec"
	ScopeVCS   = "code:vcs"
)

const (
	readTimeout  = 30 * time.Second
	writeTimeout = 30 * time.Second
	execTimeout  = 5 * time.Minute
	gitTimeout   = 30 * time.Second

	// defaultReadLines / defaultReadBytes bound one read_file call. A
	// larger file is refused at MaxFileBytes; a call within it is bounded
	// by whichever of lines/bytes is hit first.
	defaultReadLines = 500
	defaultReadBytes = 100 * 1024

	// maxExecStream caps stdout and stderr independently, so a verbose
	// build cannot exceed the tool's MaxOutput and so the generic JSON
	// truncation in tool.go never hides stderr behind a wall of stdout.
	maxExecStream = 24 * 1024

	// maxGlobMatches and maxGrepHits bound discovery results.
	maxGlobMatches = 500
	maxGrepHits    = 200
)

// Config roots the tool set. Root is required; everything else has a
// working default for a Go workspace.
type Config struct {
	Root         string   // absolute or process-relative; required
	BuildCmd     []string // default: ["go", "build", "./..."]
	TestCmd      []string // default: ["go", "test", "./..."]
	MaxFileBytes int      // default: 1 MiB; read_file refuses larger files
}

func (c Config) withDefaults() (Config, error) {
	if c.Root == "" {
		return c, fmt.Errorf("codetools: Root is required")
	}
	root, err := filepath.Abs(c.Root)
	if err != nil {
		return c, err
	}
	c.Root = root
	if len(c.BuildCmd) == 0 {
		c.BuildCmd = []string{"go", "build", "./..."}
	}
	if len(c.TestCmd) == 0 {
		c.TestCmd = []string{"go", "test", "./..."}
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = 1 << 20
	}
	return c, nil
}

// Tools returns the minimal viable self-coding tool set: twelve tools,
// five read-only, seven mutating, every mutating one RiskConfirm.
func Tools(cfg Config) ([]agent.Tool, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	return []agent.Tool{
		selfInfoTool(cfg),
		readFileTool(cfg),
		globTool(cfg),
		grepTool(cfg),
		writeFileTool(cfg),
		editFileTool(cfg),
		buildTool(cfg),
		testTool(cfg),
		gitStatusTool(cfg),
		gitBranchTool(cfg),
		gitCommitTool(cfg),
		gitRestoreTool(cfg),
		shellTool(cfg),
	}, nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// resolve turns a caller-supplied relative path into an absolute one inside
// the workspace. It is the only path guardrail; every tool that touches the
// filesystem goes through it.
func (c Config) resolve(p string) (string, error) {
	if p == "" {
		return "", kernel.NewError(kernel.CodeInvalid, "path is required")
	}
	if filepath.IsAbs(p) {
		return "", kernel.NewError(kernel.CodeInvalid, "path must be relative to the workspace root")
	}
	clean := filepath.Clean(p)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", kernel.NewError(kernel.CodeInvalid, "path escapes the workspace root")
	}
	return filepath.Join(c.Root, clean), nil
}

// rel returns the workspace-relative form of an absolute path, for output.
func (c Config) rel(p string) string {
	r, err := filepath.Rel(c.Root, p)
	if err != nil {
		return p
	}
	return r
}

// limitedBuffer caps a stream while still accepting every byte the child
// writes. The cap is invisible to the child (Write always returns len(p)),
// so a chatty process is not killed by SIGPIPE; it is merely truncated and
// flagged.
type limitedBuffer struct {
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.max <= 0 {
		b.max = maxExecStream
	}
	if b.buf.Len() >= b.max {
		b.truncated = true
		return len(p), nil
	}
	n := b.max - b.buf.Len()
	if n > len(p) {
		n = len(p)
	}
	b.buf.Write(p[:n])
	if n < len(p) {
		b.truncated = true
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

// execOutput is the shared shape for build, test and git.
type execOutput struct {
	Command   string `json:"command"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timed_out,omitempty"`
}

// runCmd executes argv in dir with the given timeout and returns capped
// streams. A non-zero exit is data (ExitCode), not an error; a launch
// failure or timeout is.
func runCmd(ctx context.Context, dir string, timeout time.Duration, argv ...string) (execOutput, error) {
	out := execOutput{Command: strings.Join(argv, " ")}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out.Stdout = stdout.String()
	out.Stderr = stderr.String()
	out.Truncated = stdout.truncated || stderr.truncated

	if err == nil {
		return out, nil
	}
	if cctx.Err() == context.DeadlineExceeded {
		out.TimedOut = true
		out.ExitCode = -1
		return out, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		out.ExitCode = exit.ExitCode()
		return out, nil
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, err
}

// matchGlob is filepath.Match with ** support: a segment of ** matches zero
// or more path segments. Every other segment falls back to path.Match.
func matchGlob(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegs(pat[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], name[0])
		if err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// ---------------------------------------------------------------------------
// self_info
// ---------------------------------------------------------------------------

var selfInfoSchema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

type selfInfoOutput struct {
	Root      string `json:"root"`
	GoVersion string `json:"go_version"`
	Commit    string `json:"commit,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Module    string `json:"module,omitempty"`
}

func selfInfoTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "self_info",
		Description: "Return the workspace root, Go version, and the current git commit, branch and Go module path. Call this first: it tells you which code you are actually running.",
		Schema:      selfInfoSchema,
		Scopes:      []string{ScopeRead},
		Risk:        agent.RiskLow,
		Timeout:     readTimeout,
		MaxOutput:   4 << 10,
	}, func(ctx context.Context, _ kernel.Principal, _ struct{}) (selfInfoOutput, error) {
		out := selfInfoOutput{Root: cfg.Root, GoVersion: runtime.Version()}
		if b, ok := gitRevParse(ctx, cfg.Root, "HEAD"); ok {
			out.Commit = b
		}
		if b, ok := gitRevParse(ctx, cfg.Root, "--abbrev-ref", "HEAD"); ok {
			out.Branch = b
		}
		if data, err := os.ReadFile(filepath.Join(cfg.Root, "go.mod")); err == nil {
			if m := moduleLine(data); m != "" {
				out.Module = m
			}
		}
		return out, nil
	})
}

func moduleLine(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == "module" {
			return f[1]
		}
	}
	return ""
}

func gitRevParse(ctx context.Context, dir string, args ...string) (string, bool) {
	argv := append([]string{"git", "rev-parse"}, args...)
	out, err := runCmd(ctx, dir, gitTimeout, argv...)
	if err != nil || out.ExitCode != 0 {
		return "", false
	}
	return strings.TrimSpace(out.Stdout), true
}

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

var readFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "File path relative to the workspace root."
    },
    "offset": {
      "type": "integer",
      "description": "1-based line to start at. Omit or 0 to start at line 1."
    },
    "limit": {
      "type": "integer",
      "description": "Maximum number of lines to return. Omit for 500."
    }
  },
  "required": ["path"]
}`)

type readFileInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type readFileOutput struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
	TotalLines int    `json:"total_lines"`
	Truncated  bool   `json:"truncated"`
}

func readFileTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "read_file",
		Description: "Read a text file from the workspace, optionally bounded by a line range. Returns numbered, line-oriented content and reports the file's total line count so you can page through a large file.",
		Schema:      readFileSchema,
		Scopes:      []string{ScopeRead},
		Risk:        agent.RiskLow,
		Timeout:     readTimeout,
		MaxOutput:   128 << 10,
	}, func(ctx context.Context, _ kernel.Principal, in readFileInput) (readFileOutput, error) {
		full, err := cfg.resolve(in.Path)
		if err != nil {
			return readFileOutput{}, err
		}
		info, err := os.Stat(full)
		if err != nil {
			return readFileOutput{}, kernel.WrapError(kernel.CodeInvalid, "file not readable", err)
		}
		if info.IsDir() {
			return readFileOutput{}, kernel.NewError(kernel.CodeInvalid, "path is a directory; use glob to list its contents")
		}
		if info.Size() > int64(cfg.MaxFileBytes) {
			return readFileOutput{}, kernel.NewError(kernel.CodeInvalid,
				fmt.Sprintf("file is %d bytes, larger than the %d-byte limit; use grep or glob to narrow", info.Size(), cfg.MaxFileBytes))
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return readFileOutput{}, kernel.WrapError(kernel.CodeInternal, "read failed", err)
		}
		if bytes.IndexByte(data, 0) >= 0 {
			return readFileOutput{}, kernel.NewError(kernel.CodeInvalid, "file appears to be binary")
		}
		lines := strings.Split(string(data), "\n")
		// A trailing newline yields a final empty element; do not count it.
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines = lines[:n-1]
		}
		total := len(lines)
		off := in.Offset
		if off <= 0 {
			off = 1
		}
		if off > total {
			return readFileOutput{Path: in.Path, Offset: off, Limit: 0, TotalLines: total, Truncated: false}, nil
		}
		limit := in.Limit
		if limit <= 0 {
			limit = defaultReadLines
		}
		end := off - 1 + limit
		if end > total {
			end = total
		}
		var b strings.Builder
		bytesBudget := defaultReadBytes
		truncated := false
		for i := off - 1; i < end; i++ {
			line := lines[i] + "\n"
			if b.Len()+len(line) > bytesBudget {
				truncated = true
				break
			}
			b.WriteString(line)
		}
		if end < total {
			truncated = true
		}
		return readFileOutput{
			Path:       in.Path,
			Content:    b.String(),
			Offset:     off,
			Limit:      limit,
			TotalLines: total,
			Truncated:  truncated,
		}, nil
	})
}

// ---------------------------------------------------------------------------
// glob
// ---------------------------------------------------------------------------

var globSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Path pattern relative to the workspace root. Supports *, ?, and ** (which matches across directory separators). Example: **/*.go"
    }
  },
  "required": ["pattern"]
}`)

type globInput struct {
	Pattern string `json:"pattern"`
}

type globOutput struct {
	Matches   []string `json:"matches"`
	Total     int      `json:"total"`
	Truncated bool     `json:"truncated"`
}

func globTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "glob",
		Description: "List files in the workspace whose path matches a glob pattern. Supports ** for recursive matching. Results are sorted and capped; the truncated flag tells you there are more.",
		Schema:      globSchema,
		Scopes:      []string{ScopeRead},
		Risk:        agent.RiskLow,
		Timeout:     readTimeout,
		MaxOutput:   32 << 10,
	}, func(ctx context.Context, _ kernel.Principal, in globInput) (globOutput, error) {
		if in.Pattern == "" {
			return globOutput{}, kernel.NewError(kernel.CodeInvalid, "pattern is required")
		}
		var matches []string
		err := filepath.WalkDir(cfg.Root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // a permission error on one entry must not abort the walk
			}
			if d.IsDir() && d.Name() == ".git" {
				return filepath.SkipDir
			}
			rel := cfg.rel(p)
			if rel == "." {
				return nil
			}
			if matchGlob(in.Pattern, rel) {
				matches = append(matches, rel)
			}
			return nil
		})
		if err != nil {
			return globOutput{}, kernel.WrapError(kernel.CodeInternal, "walk failed", err)
		}
		sort.Strings(matches)
		out := globOutput{Matches: matches, Total: len(matches)}
		if len(out.Matches) > maxGlobMatches {
			out.Matches = out.Matches[:maxGlobMatches]
			out.Truncated = true
		}
		return out, nil
	})
}

// ---------------------------------------------------------------------------
// grep
// ---------------------------------------------------------------------------

var grepSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {
      "type": "string",
      "description": "Go regular expression to search for."
    },
    "glob": {
      "type": "string",
      "description": "Optional glob limiting which files are searched, relative to the workspace root. Example: **/*.go"
    }
  },
  "required": ["pattern"]
}`)

type grepInput struct {
	Pattern string `json:"pattern"`
	Glob    string `json:"glob,omitempty"`
}

type grepHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type grepOutput struct {
	Hits      []grepHit `json:"hits"`
	Total     int       `json:"total"`
	Truncated bool      `json:"truncated"`
}

func grepTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "grep",
		Description: "Search file contents for a regular expression. Returns file, line and text for each hit, capped and sorted. Use glob to limit the search to a subtree or file type.",
		Schema:      grepSchema,
		Scopes:      []string{ScopeRead},
		Risk:        agent.RiskLow,
		Timeout:     readTimeout,
		MaxOutput:   64 << 10,
	}, func(ctx context.Context, _ kernel.Principal, in grepInput) (grepOutput, error) {
		re, err := regexp.Compile(in.Pattern)
		if err != nil {
			return grepOutput{}, kernel.WrapError(kernel.CodeInvalid, "pattern is not a valid regular expression", err)
		}
		var hits []grepHit
		walkErr := filepath.WalkDir(cfg.Root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() && d.Name() == ".git" {
				return filepath.SkipDir
			}
			if d.IsDir() {
				return nil
			}
			rel := cfg.rel(p)
			if in.Glob != "" && !matchGlob(in.Glob, rel) {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil || len(data) > cfg.MaxFileBytes {
				return nil
			}
			if bytes.IndexByte(data, 0) >= 0 {
				return nil // binary
			}
			for i, line := range strings.Split(string(data), "\n") {
				if re.MatchString(line) {
					hits = append(hits, grepHit{Path: rel, Line: i + 1, Text: line})
					if len(hits) >= maxGrepHits*4 {
						return filepath.SkipAll
					}
				}
			}
			return nil
		})
		if walkErr != nil {
			return grepOutput{}, kernel.WrapError(kernel.CodeInternal, "walk failed", walkErr)
		}
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].Path != hits[j].Path {
				return hits[i].Path < hits[j].Path
			}
			return hits[i].Line < hits[j].Line
		})
		out := grepOutput{Hits: hits, Total: len(hits)}
		if len(out.Hits) > maxGrepHits {
			out.Hits = out.Hits[:maxGrepHits]
			out.Truncated = true
		}
		return out, nil
	})
}

// ---------------------------------------------------------------------------
// write_file
// ---------------------------------------------------------------------------

var writeFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "File path relative to the workspace root. Parent directories are created as needed."
    },
    "content": {
      "type": "string",
      "description": "Complete new file contents. This replaces the file entirely; for surgical changes use edit_file."
    }
  },
  "required": ["path", "content"]
}`)

type writeFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type writeFileOutput struct {
	Path    string `json:"path"`
	Bytes   int    `json:"bytes"`
	Created bool   `json:"created"`
}

func writeFileTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "write_file",
		Description: "Create or fully replace a file. Use this for new files; for changes to an existing file prefer edit_file, which is surgical and reviewable. The write is atomic.",
		Schema:      writeFileSchema,
		Scopes:      []string{ScopeWrite},
		Risk:        agent.RiskConfirm,
		Timeout:     writeTimeout,
		MaxOutput:   4 << 10,
	}, func(ctx context.Context, _ kernel.Principal, in writeFileInput) (writeFileOutput, error) {
		full, err := cfg.resolve(in.Path)
		if err != nil {
			return writeFileOutput{}, err
		}
		if info, statErr := os.Stat(full); statErr == nil && info.IsDir() {
			return writeFileOutput{}, kernel.NewError(kernel.CodeInvalid, "path is a directory")
		}
		created := true
		if _, statErr := os.Stat(full); statErr == nil {
			created = false
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return writeFileOutput{}, kernel.WrapError(kernel.CodeInternal, "cannot create parent directory", err)
		}
		if err := atomicWrite(full, []byte(in.Content)); err != nil {
			return writeFileOutput{}, kernel.WrapError(kernel.CodeInternal, "write failed", err)
		}
		return writeFileOutput{Path: in.Path, Bytes: len(in.Content), Created: created}, nil
	})
}

// atomicWrite writes data to a temp file in the same directory and renames
// it over target, so a crash never leaves a partially written file.
func atomicWrite(target string, data []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".codetools-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, target)
}

// ---------------------------------------------------------------------------
// edit_file -- the hard one
// ---------------------------------------------------------------------------

var editFileSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "File to edit, relative to the workspace root. The file must already exist; use write_file to create one."
    },
    "edits": {
      "type": "array",
      "description": "Ordered list of exact string replacements, applied atomically.",
      "items": {
        "type": "object",
        "properties": {
          "old": {
            "type": "string",
            "description": "Exact text to replace, including whitespace. Must occur exactly once unless replace_all is true."
          },
          "new": {
            "type": "string",
            "description": "Replacement text."
          },
          "replace_all": {
            "type": "boolean",
            "description": "Replace every occurrence of old, not just the unique one. Use sparingly."
          }
        },
        "required": ["old", "new"]
      }
    }
  },
  "required": ["path", "edits"]
}`)

type editOp struct {
	Old        string `json:"old"`
	New        string `json:"new"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type editInput struct {
	Path  string   `json:"path"`
	Edits []editOp `json:"edits"`
}

type editResult struct {
	Old      string `json:"old"`
	Replaced int    `json:"replaced"`
	Skipped  bool   `json:"skipped,omitempty"`
	Note     string `json:"note,omitempty"`
}

type editFileOutput struct {
	Path    string       `json:"path"`
	Applied int          `json:"applied"`
	Skipped int          `json:"skipped"`
	Bytes   int          `json:"bytes"`
	Results []editResult `json:"results"`
}

func editFileTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "edit_file",
		Description: "Apply one or more exact string replacements to an existing file. Each replacement's old text must match exactly (including whitespace) and occur exactly once, unless replace_all is set. All edits are applied atomically: if any fails, none is written. Prefer this over write_file for changes; read the file first so your old strings are exact. If a replacement appears already applied (old absent, new present) it is skipped and reported, so a replay after a crash is safe.",
		Schema:      editFileSchema,
		Scopes:      []string{ScopeWrite},
		Risk:        agent.RiskConfirm,
		Timeout:     writeTimeout,
		MaxOutput:   16 << 10,
		// Idempotent: the already-applied skip makes a replayed step
		// converge instead of failing on a file it already changed.
		Idempotent: true,
	}, func(ctx context.Context, _ kernel.Principal, in editInput) (editFileOutput, error) {
		if len(in.Edits) == 0 {
			return editFileOutput{}, kernel.NewError(kernel.CodeInvalid, "at least one edit is required")
		}
		if len(in.Edits) > 100 {
			return editFileOutput{}, kernel.NewError(kernel.CodeInvalid, "too many edits in one call")
		}
		full, err := cfg.resolve(in.Path)
		if err != nil {
			return editFileOutput{}, err
		}
		info, err := os.Stat(full)
		if err != nil {
			return editFileOutput{}, kernel.NewError(kernel.CodeInvalid,
				"file does not exist; use write_file to create it")
		}
		if info.IsDir() {
			return editFileOutput{}, kernel.NewError(kernel.CodeInvalid, "path is a directory")
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return editFileOutput{}, kernel.WrapError(kernel.CodeInternal, "read failed", err)
		}

		original := string(data)
		current := original
		results := make([]editResult, 0, len(in.Edits))
		applied, skipped := 0, 0

		for i, op := range in.Edits {
			if op.Old == "" {
				return editFileOutput{}, kernel.NewError(kernel.CodeInvalid,
					fmt.Sprintf("edit %d: old must not be empty; use write_file to create files", i))
			}
			if op.Old == op.New {
				results = append(results, editResult{Old: op.Old, Replaced: 0, Skipped: true, Note: "old equals new"})
				skipped++
				continue
			}
			count := strings.Count(current, op.Old)
			if count == 0 {
				// Replay heuristic: if the new text is already present,
				// treat this edit as already applied. The note is
				// deliberate: the model must be able to verify.
				if strings.Contains(current, op.New) {
					results = append(results, editResult{
						Old: op.Old, Replaced: 0, Skipped: true,
						Note: "old not found but new is present; appears already applied",
					})
					skipped++
					continue
				}
				return editFileOutput{}, kernel.NewError(kernel.CodeInvalid,
					fmt.Sprintf("edit %d: old_string not found in %s; re-read the file and re-issue with the exact text", i, in.Path))
			}
			if count > 1 && !op.ReplaceAll {
				return editFileOutput{}, kernel.NewError(kernel.CodeInvalid,
					fmt.Sprintf("edit %d: old_string occurs %d times in %s; add more context or set replace_all", i, count, in.Path))
			}
			if op.ReplaceAll {
				current = strings.ReplaceAll(current, op.Old, op.New)
				results = append(results, editResult{Old: op.Old, Replaced: count})
				applied++
			} else {
				current = strings.Replace(current, op.Old, op.New, 1)
				results = append(results, editResult{Old: op.Old, Replaced: 1})
				applied++
			}
		}

		if current == original {
			return editFileOutput{
				Path: in.Path, Applied: applied, Skipped: skipped,
				Bytes: len(original), Results: results,
			}, nil
		}
		if err := atomicWrite(full, []byte(current)); err != nil {
			return editFileOutput{}, kernel.WrapError(kernel.CodeInternal, "write failed", err)
		}
		return editFileOutput{
			Path: in.Path, Applied: applied, Skipped: skipped,
			Bytes: len(current), Results: results,
		}, nil
	})
}

// ---------------------------------------------------------------------------
// build / test
// ---------------------------------------------------------------------------

var buildSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "package": {
      "type": "string",
      "description": "Go package pattern, default ./... (the whole module). Example: ./agent"
    }
  }
}`)

type buildInput struct {
	Package string `json:"package,omitempty"`
}

func buildTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "build",
		Description: "Compile a Go package pattern and return stdout, stderr and the exit code. Run this after every edit; a change that does not compile must not be committed.",
		Schema:      buildSchema,
		Scopes:      []string{ScopeExec},
		Risk:        agent.RiskLow,
		Timeout:     execTimeout,
		MaxOutput:   64 << 10,
	}, func(ctx context.Context, _ kernel.Principal, in buildInput) (execOutput, error) {
		argv := append([]string{}, cfg.BuildCmd...)
		if in.Package != "" {
			// Replace the default pattern argument; keep the toolchain
			// (go build) so a non-Go workspace can override via Config.
			argv = append(argv[:len(argv)-1], in.Package)
		}
		return runCmd(ctx, cfg.Root, execTimeout, argv...)
	})
}

var testSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "package": {
      "type": "string",
      "description": "Go package pattern, default ./... (the whole module)."
    },
    "run": {
      "type": "string",
      "description": "Optional -run filter, e.g. TestEditTool."
    },
    "verbose": {
      "type": "boolean",
      "description": "Pass -v to go test."
    }
  }
}`)

type testInput struct {
	Package string `json:"package,omitempty"`
	Run     string `json:"run,omitempty"`
	Verbose bool   `json:"verbose,omitempty"`
}

func testTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "test",
		Description: "Run Go tests and return stdout, stderr and the exit code. A non-zero exit means at least one test failed; read stderr for the failure. Test code executes the workspace's code, so treat the workspace as trusted.",
		Schema:      testSchema,
		Scopes:      []string{ScopeExec},
		Risk:        agent.RiskLow,
		Timeout:     execTimeout,
		MaxOutput:   64 << 10,
	}, func(ctx context.Context, _ kernel.Principal, in testInput) (execOutput, error) {
		argv := append([]string{}, cfg.TestCmd...)
		pkg := "./..."
		if in.Package != "" {
			pkg = in.Package
		}
		argv = append(argv[:len(argv)-1], pkg)
		if in.Verbose {
			argv = append(argv, "-v")
		}
		if in.Run != "" {
			argv = append(argv, "-run", in.Run)
		}
		return runCmd(ctx, cfg.Root, execTimeout, argv...)
	})
}

// ---------------------------------------------------------------------------
// git_status
// ---------------------------------------------------------------------------

var gitStatusSchema = json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`)

type gitStatusOutput struct {
	Branch    string   `json:"branch"`
	Clean     bool     `json:"clean"`
	Staged    []string `json:"staged"`
	Unstaged  []string `json:"unstaged"`
	Untracked []string `json:"untracked"`
	Diff      string   `json:"diff,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

func gitStatusTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "git_status",
		Description: "Show the current branch, staged / unstaged / untracked files, and the working-tree diff. Call this before committing to see exactly what will be committed.",
		Schema:      gitStatusSchema,
		Scopes:      []string{ScopeRead, ScopeVCS},
		Risk:        agent.RiskLow,
		Timeout:     gitTimeout,
		MaxOutput:   64 << 10,
	}, func(ctx context.Context, _ kernel.Principal, _ struct{}) (gitStatusOutput, error) {
		out := gitStatusOutput{Clean: true}
		if b, ok := gitRevParse(ctx, cfg.Root, "--abbrev-ref", "HEAD"); ok {
			out.Branch = b
		}
		status, err := runCmd(ctx, cfg.Root, gitTimeout, "git", "status", "--porcelain")
		if err != nil {
			return gitStatusOutput{}, err
		}
		if status.ExitCode != 0 {
			return gitStatusOutput{}, kernel.NewError(kernel.CodeInternal, "git status failed: "+status.Stderr)
		}
		for _, line := range strings.Split(status.Stdout, "\n") {
			if len(line) < 4 {
				continue
			}
			x, y, file := line[0], line[1], line[3:]
			switch {
			case x == '?' && y == '?':
				out.Untracked = append(out.Untracked, file)
			default:
				if x != ' ' && x != '?' {
					out.Staged = append(out.Staged, file)
				}
				if y != ' ' && y != '?' {
					out.Unstaged = append(out.Unstaged, file)
				}
			}
		}
		if len(out.Staged)+len(out.Unstaged)+len(out.Untracked) > 0 {
			out.Clean = false
			diff, _ := runCmd(ctx, cfg.Root, gitTimeout, "git", "diff", "HEAD")
			out.Diff = diff.Stdout
			out.Truncated = diff.Truncated
		}
		return out, nil
	})
}

// ---------------------------------------------------------------------------
// git_branch
// ---------------------------------------------------------------------------

var gitBranchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {
      "type": "string",
      "description": "New branch name. A branch is the cheapest rollback you have; never edit on the default branch."
    }
  },
  "required": ["name"]
}`)

type gitBranchInput struct {
	Name string `json:"name"`
}

type gitBranchOutput struct {
	Name   string `json:"name"`
	Parent string `json:"parent,omitempty"`
}

func gitBranchTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "git_branch",
		Description: "Create and switch to a new branch. Make a branch before making edits; a bad change is then one checkout away.",
		Schema:      gitBranchSchema,
		Scopes:      []string{ScopeVCS},
		Risk:        agent.RiskConfirm,
		Timeout:     gitTimeout,
		MaxOutput:   4 << 10,
	}, func(ctx context.Context, _ kernel.Principal, in gitBranchInput) (gitBranchOutput, error) {
		if in.Name == "" {
			return gitBranchOutput{}, kernel.NewError(kernel.CodeInvalid, "name is required")
		}
		parent, _ := gitRevParse(ctx, cfg.Root, "--abbrev-ref", "HEAD")
		out, err := runCmd(ctx, cfg.Root, gitTimeout, "git", "checkout", "-b", in.Name)
		if err != nil {
			return gitBranchOutput{}, err
		}
		if out.ExitCode != 0 {
			return gitBranchOutput{}, kernel.NewError(kernel.CodeInternal, "git checkout -b failed: "+out.Stderr)
		}
		return gitBranchOutput{Name: in.Name, Parent: parent}, nil
	})
}

// ---------------------------------------------------------------------------
// git_commit
// ---------------------------------------------------------------------------

var gitCommitSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "message": {
      "type": "string",
      "description": "Commit message. State what changed and why."
    }
  },
  "required": ["message"]
}`)

type gitCommitInput struct {
	Message string `json:"message"`
}

type gitCommitOutput struct {
	Commit  string `json:"commit"`
	Summary string `json:"summary"`
}

func gitCommitTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "git_commit",
		Description: "Stage all working-tree changes and commit with the given message. Run build and test first; a commit is the reviewable unit. Returns the new commit hash.",
		Schema:      gitCommitSchema,
		Scopes:      []string{ScopeVCS},
		Risk:        agent.RiskConfirm,
		Timeout:     gitTimeout,
		MaxOutput:   8 << 10,
	}, func(ctx context.Context, _ kernel.Principal, in gitCommitInput) (gitCommitOutput, error) {
		if in.Message == "" {
			return gitCommitOutput{}, kernel.NewError(kernel.CodeInvalid, "message is required")
		}
		if out, err := runCmd(ctx, cfg.Root, gitTimeout, "git", "add", "-A"); err != nil || out.ExitCode != 0 {
			return gitCommitOutput{}, kernel.NewError(kernel.CodeInternal, "git add failed: "+out.Stderr)
		}
		out, err := runCmd(ctx, cfg.Root, gitTimeout, "git", "commit", "-m", in.Message)
		if err != nil {
			return gitCommitOutput{}, err
		}
		if out.ExitCode != 0 {
			return gitCommitOutput{}, kernel.NewError(kernel.CodeInternal, "git commit failed: "+out.Stderr)
		}
		hash, _ := gitRevParse(ctx, cfg.Root, "HEAD")
		return gitCommitOutput{Commit: hash, Summary: strings.TrimSpace(out.Stdout)}, nil
	})
}

// ---------------------------------------------------------------------------
// git_restore
// ---------------------------------------------------------------------------

var gitRestoreSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {
      "type": "string",
      "description": "Path to restore, relative to the workspace root. Default: . (all files)."
    },
    "ref": {
      "type": "string",
      "description": "Commit, branch or tag to restore from. Default: HEAD."
    }
  }
}`)

type gitRestoreInput struct {
	Path string `json:"path,omitempty"`
	Ref  string `json:"ref,omitempty"`
}

type gitRestoreOutput struct {
	Path string `json:"path"`
	Ref  string `json:"ref"`
}

func gitRestoreTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "git_restore",
		Description: "Discard working-tree changes by restoring files from a ref (default HEAD). This is destructive: uncommitted changes to the named paths are lost. Use it to undo a bad edit. It does not remove untracked files.",
		Schema:      gitRestoreSchema,
		Scopes:      []string{ScopeVCS},
		Risk:        agent.RiskConfirm,
		Timeout:     gitTimeout,
		MaxOutput:   4 << 10,
	}, func(ctx context.Context, _ kernel.Principal, in gitRestoreInput) (gitRestoreOutput, error) {
		p := in.Path
		if p == "" {
			p = "."
		}
		if _, err := cfg.resolve(p); err != nil {
			return gitRestoreOutput{}, err
		}
		ref := in.Ref
		if ref == "" {
			ref = "HEAD"
		}
		out, err := runCmd(ctx, cfg.Root, gitTimeout, "git", "checkout", ref, "--", p)
		if err != nil {
			return gitRestoreOutput{}, err
		}
		if out.ExitCode != 0 {
			return gitRestoreOutput{}, kernel.NewError(kernel.CodeInternal, "git checkout failed: "+out.Stderr)
		}
		return gitRestoreOutput{Path: p, Ref: ref}, nil
	})
}

const shellTimeout = 2 * time.Minute

var shellSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {
      "type": "string",
      "description": "Shell command line, run with sh -c. Prefer one command per call. Anything longer than about 16 KiB should be written to a file and run as sh script.sh."
    },
    "workdir": {
      "type": "string",
      "description": "Working directory relative to the workspace root. Omit for the root. Absolute paths and paths that escape the root are refused. This is a starting directory, not a sandbox: the command has the same filesystem access as the agent process."
    }
  },
  "required": ["command"]
}`)

type shellInput struct {
	Command string `json:"command"`
	Workdir string `json:"workdir,omitempty"`
}

type shellOutput struct {
	Command   string `json:"command"`
	Workdir   string `json:"workdir"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	Truncated bool   `json:"truncated,omitempty"`
	TimedOut  bool   `json:"timed_out,omitempty"`
}

func shellTool(cfg Config) agent.Tool {
	return agent.NewTool(agent.ToolSpec{
		Name:        "shell",
		Description: "Run a shell command with sh -c in the workspace and return stdout, stderr and the exit code. This is the escape hatch for work the typed tools do not cover: mkdir, find, sed, process management, or git operations outside the dedicated git tools. Prefer the typed tools for anything they can do, because they validate arguments and return structured results. Every command requires human approval; there is no sandbox, so approval is the only guard, and the command runs with the agent process's privileges.",
		Schema:      shellSchema,
		Scopes:      []string{ScopeExec},
		Risk:        agent.RiskConfirm,
		Timeout:     shellTimeout,
		MaxOutput:   128 << 10,
	}, func(ctx context.Context, _ kernel.Principal, in shellInput) (shellOutput, error) {
		if in.Command == "" {
			return shellOutput{}, kernel.NewError(kernel.CodeInvalid, "command is required")
		}
		if len(in.Command) > 16*1024 {
			return shellOutput{}, kernel.NewError(kernel.CodeInvalid,
				"command is too long (max 16 KiB); write a script with write_file and run it with `sh script.sh`")
		}
		dir := cfg.Root
		if in.Workdir != "" {
			resolved, err := cfg.resolve(in.Workdir)
			if err != nil {
				return shellOutput{}, err
			}
			dir = resolved
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return shellOutput{}, kernel.NewError(kernel.CodeInvalid, "workdir is not a directory")
		}
		return runShell(ctx, dir, in.Command)
	})
}

// runShell executes command with sh -c in dir. It differs from runCmd in
// two ways that matter for arbitrary commands:
//
//   - The child is placed in its own process group (Setpgid) and
//     cancellation sends SIGKILL to the whole group (-pid), so a shell that
//     backgrounds work cannot escape the timeout by orphaning grandchildren.
//     WaitDelay forces Wait to return even if a grandchild survives SIGKILL
//     in an uninterruptible state.
//
//   - The streams are capped by limitedBuffer, so a runaway command cannot
//     exhaust memory before MaxOutput fires, and so the generic truncation
//     in tool.go never hides stderr or exit_code behind a wall of stdout.
//
// Cancellation by the framework (not the tool's own timeout) propagates as
// the context error, so the step sees CodeCanceled rather than a synthetic
// exit code.
func runShell(ctx context.Context, dir, command string) (shellOutput, error) {
	out := shellOutput{Command: command, Workdir: dir}
	cctx, cancel := context.WithTimeout(ctx, shellTimeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Kill the group, not just the shell. Ignore ESRCH: the shell may
		// have exited between cancellation and this call, and a dead group
		// is what we wanted anyway.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return nil
	}
	cmd.WaitDelay = 5 * time.Second

	var stdout, stderr limitedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	out.Stdout = stdout.String()
	out.Stderr = stderr.String()
	out.Truncated = stdout.truncated || stderr.truncated

	if err == nil {
		return out, nil
	}
	if cctx.Err() == context.DeadlineExceeded {
		out.TimedOut = true
		out.ExitCode = -1
		return out, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		out.ExitCode = exit.ExitCode()
		return out, nil
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, err
}
