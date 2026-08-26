// Package parser discovers, parses, normalizes, and validates hook definition
// files under .discobox/hooks.
package parser

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	hooks "github.com/discobox-ai/hooks"
	"github.com/discobox-ai/x/frontmatter"
)

const (
	HooksDirName     = ".discobox/hooks"
	GlobalIgnoreName = "ignore"
)

var phaseNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Discovery is the result of discovering a repository's hook directory.
type Discovery struct {
	Hooks               []hooks.Hook `json:"hooks"`
	GlobalIgnore        []string     `json:"global_ignore,omitempty"`
	HooksDir            string       `json:"hooks_dir"`
	GlobalIgnoreAbsPath string       `json:"global_ignore_abs_path,omitempty"`
}

// ValidationError identifies a hook validation problem and, when known, the
// offending field and path.
type ValidationError struct {
	Path  string
	Field string
	Err   error
}

func (e *ValidationError) Error() string {
	if e == nil {
		return ""
	}
	parts := make([]string, 0, 3)
	if e.Path != "" {
		parts = append(parts, e.Path)
	}
	if e.Field != "" {
		parts = append(parts, e.Field)
	}
	if e.Err != nil {
		parts = append(parts, e.Err.Error())
	}
	return strings.Join(parts, ": ")
}

func (e *ValidationError) Unwrap() error { return e.Err }

// ValidationErrors collects more than one hook validation failure.
type ValidationErrors []error

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return ""
	}
	msgs := make([]string, len(e))
	for i := range e {
		msgs[i] = e[i].Error()
	}
	return strings.Join(msgs, "; ")
}

func (e ValidationErrors) Unwrap() []error { return []error(e) }

func fieldError(path, field string, format string, args ...any) error {
	return &ValidationError{Path: path, Field: field, Err: fmt.Errorf(format, args...)}
}

// Discover reads direct hook files from repoRoot/.discobox/hooks. An absent hooks
// directory is not an error.
func Discover(repoRoot string) (*Discovery, error) {
	repoRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, err
	}
	hooksDir := filepath.Join(repoRoot, HooksDirName)
	out := &Discovery{HooksDir: hooksDir}
	entries, err := os.ReadDir(hooksDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return out, nil
		}
		return nil, err
	}

	var errs ValidationErrors
	for _, entry := range entries {
		name := entry.Name()
		if name == GlobalIgnoreName && !entry.IsDir() {
			ignorePath := filepath.Join(hooksDir, name)
			patterns, err := ParseGlobalIgnoreFile(ignorePath)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			out.GlobalIgnore = patterns
			out.GlobalIgnoreAbsPath = ignorePath
			continue
		}
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(hooksDir, name)
		hook, err := ParseFile(repoRoot, path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out.Hooks = append(out.Hooks, hook)
	}

	sort.Slice(out.Hooks, func(i, j int) bool {
		if out.Hooks[i].Name == out.Hooks[j].Name {
			return out.Hooks[i].ID < out.Hooks[j].ID
		}
		return out.Hooks[i].Name < out.Hooks[j].Name
	})
	if len(errs) > 0 {
		return out, errs
	}
	return out, nil
}

// ParseGlobalIgnoreFile parses .discobox/hooks/ignore.
func ParseGlobalIgnoreFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, filepath.ToSlash(line))
	}
	return patterns, nil
}

// ParseFile parses and validates a single hook file.
func ParseFile(repoRoot, path string) (hooks.Hook, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return hooks.Hook{}, err
	}
	repoRoot, err = filepath.Abs(repoRoot)
	if err != nil {
		return hooks.Hook{}, err
	}
	data, err := os.ReadFile(absPath)
	if err != nil {
		return hooks.Hook{}, err
	}
	mode := fs.FileMode(0)
	if info, err := os.Stat(absPath); err == nil {
		mode = info.Mode()
	}
	rel, err := filepath.Rel(repoRoot, absPath)
	if err != nil {
		rel = absPath
	}
	rel = filepath.ToSlash(rel)

	parsed, err := frontmatter.Parse(data)
	if err != nil {
		return hooks.Hook{}, &ValidationError{Path: rel, Field: "front_matter", Err: err}
	}
	fields, err := frontmatter.Decode(parsed.Meta)
	if err != nil {
		return hooks.Hook{}, &ValidationError{Path: rel, Field: "front_matter", Err: err}
	}
	return buildHook(rel, absPath, filepath.Base(path), data, mode, parsed, metadataFrom(fields))
}

// metadata is the hook domain's reading of a decoded front-matter block: the
// keys hooks give meaning to, in the shape hooks want them, with everything
// else kept as extensions.
//
// The spelling of keys and the shape of values are frontmatter's; what is
// lowercased, what is a path, and which spellings are aliases of one field are
// hook decisions and live here.
type metadata struct {
	Name        string
	Type        string
	Description string
	Engine      string
	RunAs       string
	Blocking    bool
	Pattern     string
	Ignore      []string
	Phase       string
	Subagent    string
	LanguageID  string
	MinSeverity string
	Extensions  map[string]any
}

// knownFields are the keys metadataFrom consumes, and so the ones held back
// from Extensions. Aliases are listed too: a file that wrote `exclude` has had
// it read, and repeating it as an extension would report it as unrecognized.
var knownFields = []string{
	"name", "type", "description", "engine", "run_as", "blocking",
	"pattern", "ignore", "exclude", "phase", "subagent",
	"language_id", "language", "min_severity", "minimum_severity",
}

func metadataFrom(fields frontmatter.Fields) metadata {
	m := metadata{
		Name:        fields.String("name"),
		Type:        strings.ToLower(fields.String("type")),
		Description: fields.String("description"),
		Engine:      strings.ToLower(fields.String("engine")),
		RunAs:       strings.ToLower(fields.String("run_as")),
		Blocking:    fields.Bool("blocking"),
		Pattern:     filepath.ToSlash(fields.String("pattern")),
		Ignore:      fields.Strings("ignore", "exclude"),
		Phase:       strings.ToLower(fields.String("phase")),
		Subagent:    fields.String("subagent"),
		LanguageID:  fields.String("language_id", "language"),
		MinSeverity: strings.ToLower(fields.String("min_severity", "minimum_severity")),
		Extensions:  fields.Except(knownFields...),
	}
	for i := range m.Ignore {
		m.Ignore[i] = filepath.ToSlash(m.Ignore[i])
	}
	if m.Extensions == nil {
		m.Extensions = map[string]any{}
	}
	return m
}

func buildHook(rel, abs, filename string, data []byte, mode fs.FileMode, pf frontmatter.Parsed, m metadata) (hooks.Hook, error) {
	id := frontmatter.NormalizeID(filename)
	if id == "" {
		return hooks.Hook{}, fieldError(rel, "id", "filename cannot produce stable hook ID")
	}
	name := strings.TrimSpace(m.Name)
	if name == "" {
		name = frontmatter.DefaultName(filename)
	}
	engine := hooks.HookEngine(m.Engine)
	if engine == "" {
		engine = hooks.HookEngineScript
	}
	runAs := hooks.RunAs(m.RunAs)
	if runAs == "" {
		runAs = hooks.RunAsUser
	}
	h := hooks.Hook{
		ID: id, Name: name, Description: m.Description,
		Type: hooks.HookType(m.Type), Engine: engine, RunAs: runAs, Blocking: m.Blocking,
		Pattern: m.Pattern, Ignore: m.Ignore, Phase: m.Phase,
		Subagent: m.Subagent, LanguageID: m.LanguageID, MinSeverity: m.MinSeverity,
		AbsPath: abs, RelPath: rel, HasShebang: pf.HasShebang,
		Executable: mode&0111 != 0, Extensions: m.Extensions,
	}
	if len(h.Extensions) == 0 {
		h.Extensions = nil
	}
	if h.Engine == hooks.HookEngineAI {
		h.Prompt = strings.TrimSpace(pf.Body)
	}
	if err := validateHook(h, data); err != nil {
		return hooks.Hook{}, err
	}
	return h, nil
}

func validateHook(h hooks.Hook, data []byte) error {
	if h.Type == "" {
		return fieldError(h.RelPath, "type", "missing required field")
	}
	if !h.Type.Valid() {
		return fieldError(h.RelPath, "type", "unsupported hook type %q", h.Type)
	}
	if h.Engine == "" || !h.Engine.Valid() {
		return fieldError(h.RelPath, "engine", "unsupported hook engine %q", h.Engine)
	}
	if h.Engine == hooks.HookEngineBuiltin {
		return fieldError(h.RelPath, "engine", "builtin hooks require a registered implementation")
	}
	if !h.RunAs.Valid() {
		return fieldError(h.RelPath, "run_as", "unsupported run_as %q", h.RunAs)
	}
	if h.Type == hooks.HookTypeFile && strings.TrimSpace(h.Pattern) == "" {
		return fieldError(h.RelPath, "pattern", "file hooks require pattern")
	}
	if h.Phase != "" {
		if h.Phase == "all" {
			return fieldError(h.RelPath, "phase", "phase %q is reserved", h.Phase)
		}
		if !phaseNamePattern.MatchString(h.Phase) {
			return fieldError(h.RelPath, "phase", "invalid phase %q: use lowercase letters, digits, hyphens, and underscores", h.Phase)
		}
	}
	if h.Engine == hooks.HookEngineScript {
		if !frontmatter.HasShebangLine(data) {
			return fieldError(h.RelPath, "shebang", "script hooks require shebang as first line")
		}
		if runtime.GOOS != "windows" && !h.Executable {
			return fieldError(h.RelPath, "mode", "script hooks must be executable")
		}
	}
	if h.Engine == hooks.HookEngineLSP {
		if !frontmatter.HasShebangLine(data) {
			return fieldError(h.RelPath, "shebang", "lsp hooks require shebang as first line")
		}
		if runtime.GOOS != "windows" && !h.Executable {
			return fieldError(h.RelPath, "mode", "lsp hooks must be executable")
		}
		if strings.TrimSpace(h.LanguageID) == "" {
			return fieldError(h.RelPath, "language_id", "lsp hooks require language_id")
		}
		if h.Type != hooks.HookTypeFile {
			return fieldError(h.RelPath, "type", "lsp hooks require type file")
		}
		switch h.MinSeverity {
		case "", "error", "warning", "information", "info", "hint":
		default:
			return fieldError(h.RelPath, "min_severity", "unsupported severity %q", h.MinSeverity)
		}
	}
	return nil
}
